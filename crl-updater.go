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
	"net"
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

const (
	X509CRLPEMHeader string = "-----BEGIN X509 CRL-----"
)

type (
	// Global metrics
	Metrics struct {
		Success      *prometheus.CounterVec
		Error        *prometheus.CounterVec
		SuccessTotal prometheus.Counter
		ErrorTotal   prometheus.Counter
	}

	// Main configuration
	Config struct {
		CRLJobs []*CRLJob `yaml:"jobs"`
	}

	// CRL update job definition
	CRLJob struct {
		ID cron.EntryID

		// Source URL to download the CRL from
		Source string `yaml:"src"`
		// Destination file to save the CRL to
		Destination string `yaml:"dest"`
		// CRL update job cron schedule
		Schedule string `yaml:"schedule"`
		// CRL file size limit
		SizeLimit int64 `yaml:"limit"`
		// CRL download attempt timeout (human readable)
		TimeoutHuman string `yaml:"timeout"`
		// CRL download attempt timeout (time.Duration)
		TimeoutDuration time.Duration

		// Global metrics to update from each job
		Metrics *Metrics
	}
)

// Runs this CRL update job
func (j *CRLJob) Run() {
	// Create a temporary file for the CRL
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

	// Download the CRL, compute its checksum
	if err := downloadCRL(j.Source, tempWriter, tempHash, j.TimeoutDuration, j.SizeLimit); err != nil {
		log.Printf("[%v] [%s]: failed to download CRL: %v", j.ID, j.Destination, err)
		j.Metrics.ErrorTotal.Inc()
		j.Metrics.Error.With(prometheus.Labels{"job": fmt.Sprintf("%v", j.ID), "file": j.Destination}).Inc()
		return
	}

	// Open the destination file to check if the CRL has changed
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

		// No changes in the CRL, job is done
		if bytes.Equal(tempHash.Sum(nil), destHash.Sum(nil)) {
			log.Printf("[%v] [%s]: CRL source did not change", j.ID, j.Destination)
			j.Metrics.SuccessTotal.Inc()
			j.Metrics.Success.With(prometheus.Labels{"job": fmt.Sprintf("%v", j.ID), "file": j.Destination}).Inc()
			return
		}

		// Close the destination file because Windows doesn't like replacing opened files
		destFile.Close()
	}

	// Replace the destination file atomically
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

// Download CRL file and compute its checksum
func downloadCRL(url string, w *bufio.Writer, h hash.Hash, timeout time.Duration, limit int64) error {
	c := &http.Client{Timeout: timeout, Transport: &http.Transport{DisableKeepAlives: true, DialContext: (&net.Dialer{KeepAlive: -1}).DialContext}}
	r, err := c.Get(url)
	if r != nil {
		defer r.Body.Close()
	}
	if err != nil {
		return errors.Wrap(err, "http request failed")
	}

	// Write the destination file and its hash
	dest := io.MultiWriter(w, h)

	// Read a small fragment of the response body first
	head := make([]byte, 24)
	if _, err := io.ReadFull(r.Body, head); err != nil {
		return errors.Wrap(err, "head read failed")
	}

	// Check if we're being offered a CRL file
	if !isCRL(head) {
		return errors.New("source is not a DER or PEM encoded CRL")
	}

	// Read the first fragment and the remainder of the body
	src := io.MultiReader(bytes.NewReader(head), http.MaxBytesReader(nil, r.Body, limit-int64(24)))

	// Copy data to destination and flush it
	if _, err = io.Copy(dest, src); err != nil {
		return errors.Wrap(err, "copy failed")
	}
	if err := w.Flush(); err != nil {
		return errors.Wrap(err, "flush failed")
	}

	return nil
}

// Check if passed byte slice is a beginning of a DER or PEM encoded CRL
func isCRL(b []byte) bool {
	return string(b) == X509CRLPEMHeader || (b[0] == 0x30 && (b[1] == 0x82 || b[1] == 0x83))
}

// Load and unmarshal YAML in the config file
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

	// Create Prometheus metrics
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

	for _, job := range jobs {
		if job.Source == "" || job.Destination == "" {
			log.Printf("empty source (%s) or destination (%s), skipping job", job.Source, job.Destination)
			continue
		}
		// Validate schedule (if not specified/invalid)
		if _, err := cron.ParseStandard(job.Schedule); err != nil {
			log.Printf("[%s]: failed to parse job schedule, assuming default (@hourly): %v", job.Destination, err)
			job.Schedule = "@hourly"
		}
		// Validate download attempt timeout (if not specified/invalid)
		if job.TimeoutDuration, err = time.ParseDuration(job.TimeoutHuman); err != nil {
			log.Printf("[%s]: failed to parse job timeout, assuming default (1m): %v", job.Destination, err)
			job.TimeoutDuration = time.Second * 60
		}
		// Validate CRL size limit
		if job.SizeLimit <= 0 {
			log.Printf("[%s]: invalid CRL size limit, assuming default (10MiB)", job.Destination)
			job.SizeLimit = 10485760
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

	// Serve metrics
	http.Handle("/metrics", promhttp.HandlerFor(pmReg, promhttp.HandlerOpts{}))
	if err := http.ListenAndServe(*metricsAddr, nil); err != nil {
		log.Fatal(err)
	}
}
