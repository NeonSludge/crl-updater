package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"flag"
	"fmt"
	"hash"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/renameio"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

var (
	X509CRLPEMHeader string = "-----BEGIN X509 CRL-----"
	X509CRLDERMagic1 []byte = []byte{48, 130}
	X509CRLDERMagic2 []byte = []byte{48, 131}
)

type Metrics struct {
	Success      *prometheus.CounterVec
	Error        *prometheus.CounterVec
	SuccessTotal prometheus.Counter
	ErrorTotal   prometheus.Counter
}

// CRL update job definition
type CRLJob struct {
	ID cron.EntryID

	Source          string `yaml:"src"`
	Destination     string `yaml:"dest"`
	Schedule        string `yaml:"schedule"`
	TimeoutHuman    string `yaml:"timeout"`
	TimeoutDuration time.Duration

	Metrics *Metrics
}

// Runs this CRL update job
func (j *CRLJob) Run() {
	tempFile, err := renameio.TempFile(renameio.TempDir(filepath.Dir(j.Destination)), j.Destination)
	if err != nil {
		log.Printf("[%v] [%s]: failed to create a temporary file: %v", j.ID, j.Destination, err)
		j.Metrics.ErrorTotal.Inc()
		j.Metrics.Error.With(prometheus.Labels{"job": fmt.Sprintf("%v", j.ID), "file": j.Destination}).Inc()
		return
	}
	defer tempFile.Cleanup()

	tempWriter := bufio.NewWriter(tempFile)
	tempHash := sha256.New()

	if err := downloadCRL(j.Source, tempWriter, tempHash, j.TimeoutDuration); err != nil {
		log.Printf("[%v] [%s]: failed to download CRL: %v", j.ID, j.Destination, err)
		j.Metrics.ErrorTotal.Inc()
		j.Metrics.Error.With(prometheus.Labels{"job": fmt.Sprintf("%v", j.ID), "file": j.Destination}).Inc()
		return
	}

	destFile, err := os.Open(j.Destination)
	if err == nil {
		defer destFile.Close()

		destHash := sha256.New()
		if _, err := io.Copy(destHash, bufio.NewReader(destFile)); err != nil {
			log.Printf("[%v] [%s]: failed to compare CRL files: %v", j.ID, j.Destination, err)
			j.Metrics.ErrorTotal.Inc()
			j.Metrics.Error.With(prometheus.Labels{"job": fmt.Sprintf("%v", j.ID), "file": j.Destination}).Inc()
			return
		}

		if bytes.Equal(tempHash.Sum(nil), destHash.Sum(nil)) {
			log.Printf("[%v] [%s]: CRL source did not change", j.ID, j.Destination)
			j.Metrics.SuccessTotal.Inc()
			j.Metrics.Success.With(prometheus.Labels{"job": fmt.Sprintf("%v", j.ID), "file": j.Destination}).Inc()
			return
		}

		destFile.Close()
	}

	if err := tempFile.CloseAtomicallyReplace(); err != nil {
		log.Printf("[%v] [%s]: failed to replace existing CRL file: %v", j.ID, j.Destination, err)
		j.Metrics.ErrorTotal.Inc()
		j.Metrics.Error.With(prometheus.Labels{"job": fmt.Sprintf("%v", j.ID), "file": j.Destination}).Inc()
		return
	}

	log.Printf("[%v] [%s]: updated target CRL file", j.ID, j.Destination)
	j.Metrics.SuccessTotal.Inc()
	j.Metrics.Success.With(prometheus.Labels{"job": fmt.Sprintf("%v", j.ID), "file": j.Destination}).Inc()
}

// Config file structure
type Config struct {
	CRLJobs []*CRLJob `yaml:"jobs"`
}

// Unmarshal YAML in the config file
func makeConfig(r *bufio.Reader) (*Config, error) {
	b, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, errors.Wrap(err, "config file loading failed")
	}

	cfg := &Config{}
	err = yaml.Unmarshal(b, cfg)
	if err != nil {
		return nil, errors.Wrap(err, "YAML unmarshalling failed")
	}

	return cfg, nil
}

// Download CRL file from the specified location and write it to the specified Writer
func downloadCRL(src string, w *bufio.Writer, h hash.Hash, timeout time.Duration) error {
	c := &http.Client{Timeout: timeout}
	r, err := c.Get(src)
	if r != nil {
		defer r.Body.Close()
		defer io.Copy(ioutil.Discard, r.Body)
	}
	if err != nil {
		return errors.Wrap(err, "http request failed")
	}

	dest := io.MultiWriter(w, h)

	head := make([]byte, 24)
	if _, err := io.ReadFull(r.Body, head); err != nil {
		return errors.Wrap(err, "head read failed")
	}

	if err := validateHead(head); err != nil {
		return errors.Wrap(err, "head validation failed")
	}

	if _, err = io.Copy(dest, bytes.NewReader(head)); err != nil {
		return errors.Wrap(err, "head copy failed")
	}

	if _, err := io.Copy(dest, r.Body); err != nil {
		return errors.Wrap(err, "body copy failed")
	}

	if err := w.Flush(); err != nil {
		return errors.Wrap(err, "temp writer flush failed")
	}

	return nil
}

// Validate received response fragment
func validateHead(b []byte) error {
	if string(b) != X509CRLPEMHeader {
		if !(bytes.Equal(b[:2], X509CRLDERMagic1) || bytes.Equal(b[:2], X509CRLDERMagic2)) {
			return errors.New("not a PEM or DER encoded file")
		}
	}

	return nil
}

func main() {
	// cmd-line arguments
	cfgPath := flag.String("cfg", "/etc/crl-updater.yaml", "path to a config file in YAML format")
	metricsAddr := flag.String("metrics", ":8080", "address for publishing metrics in Prometheus format")
	flag.Parse()

	cfgFile, err := os.Open(*cfgPath)
	if err != nil {
		log.Fatal(errors.Wrap(err, "config file opening failed"))
	}

	// Unmarshal config file
	cfg, err := makeConfig(bufio.NewReader(cfgFile))
	if err != nil {
		log.Fatal(errors.Wrap(err, "config file parsing failed"))
	}
	cfgFile.Close()

	pmMetrics := &Metrics{
		Success: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "crl_updater_success",
			Help: "Number of successful CRL update attempts per job.",
		}, []string{"job", "file"}),
		Error: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "crl_updater_error",
			Help: "Number of unsuccessful CRL update attempts per job.",
		}, []string{"job", "file"}),
		SuccessTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "crl_updater_success_total",
			Help: "Number of successful CRL update attempts.",
		}),
		ErrorTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "crl_updater_error_total",
			Help: "Number of unsuccessful CRL update attempts.",
		}),
	}

	pmReg := prometheus.NewRegistry()
	pmReg.MustRegister(pmMetrics.Success)
	pmReg.MustRegister(pmMetrics.Error)
	pmReg.MustRegister(pmMetrics.SuccessTotal)
	pmReg.MustRegister(pmMetrics.ErrorTotal)

	sched := cron.New()
	jobs := cfg.CRLJobs
	// Iterate on job list
	for _, job := range jobs {
		if job.Source == "" || job.Destination == "" {
			log.Printf("empty source (%s) or destination (%s), skipping job", job.Source, job.Destination)
			continue
		}
		// Default schedule (if not specified/invalid)
		if _, err := cron.ParseStandard(job.Schedule); err != nil {
			log.Printf("[%s]: failed to parse job schedule, assuming default: %v", job.Destination, err)
			job.Schedule = "@hourly"
		}
		// Default timeout (if not specified/invalid)
		if job.TimeoutDuration, err = time.ParseDuration(job.TimeoutHuman); err != nil {
			log.Printf("[%s]: failed to parse job timeout, assuming default: %v", job.Destination, err)
			job.TimeoutDuration = time.Second * 60
		}
		// Add job to scheduler
		id, err := sched.AddJob(job.Schedule, job)
		if err != nil {
			log.Printf("[%s]: failed to add CRL update job: %v", job.Destination, err)
			continue
		}
		job.ID = id
		job.Metrics = pmMetrics
		log.Printf("[%v] [%s]: added CRL update job", job.ID, job.Destination)
	}
	// Run jobs
	sched.Start()

	for {
		http.Handle("/metrics", promhttp.HandlerFor(pmReg, promhttp.HandlerOpts{}))
		if err := http.ListenAndServe(*metricsAddr, nil); err != nil {
			log.Fatal(err)
		}
	}
}
