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
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/NeonSludge/crl-updater/pkg/utils"
	"github.com/google/renameio"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v3"
)

const (
	X509CRLPEMHeader string = "-----BEGIN X509 CRL-----"

	DefaultTimeoutDuration time.Duration = time.Minute
	DefaultSizeLimit       int64         = 10485760
	DefaultSchedule        string        = "@hourly"
	DefaultFileMode        uint32        = 0644
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
		URL string `yaml:"url"`
		// Destination file to save the CRL to
		Destination string `yaml:"dest"`
		// Desired file permissions for the CRL file
		Mode uint32 `yaml:"mode"`
		// Desired owner of the CRL file
		Owner string `yaml:"owner"`
		UID   int
		// Desired group of the CRL file
		Group string `yaml:"group"`
		GID   int
		// Force CRL file update, skip all checks
		ForceUpdate bool `yaml:"force"`
		// CRL update job cron schedule
		Schedule string `yaml:"schedule"`
		// CRL file size limit
		SizeLimit int64 `yaml:"limit"`
		// CRL download attempt timeout
		TimeoutHuman    string `yaml:"timeout"`
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
		log.Error().Interface("id", j.ID).Str("dest", j.Destination).Str("url", j.URL).Err(err).Msg("failed to create a temporary file")
		j.Metrics.ErrorTotal.Inc()
		j.Metrics.Error.With(prometheus.Labels{"job": fmt.Sprintf("%v", j.ID), "file": j.Destination}).Inc()
		return
	}
	defer tempFile.Cleanup()

	tempHash := sha256.New()

	// Download the CRL, compute its checksum
	if err := downloadCRL(j.URL, tempFile, tempHash, j.TimeoutDuration, j.SizeLimit, j.ForceUpdate); err != nil {
		log.Error().Interface("id", j.ID).Str("dest", j.Destination).Str("url", j.URL).Err(err).Msg("failed to download CRL")
		j.Metrics.ErrorTotal.Inc()
		j.Metrics.Error.With(prometheus.Labels{"job": fmt.Sprintf("%v", j.ID), "file": j.Destination}).Inc()
		return
	}

	if !j.ForceUpdate {
		// Open the destination file to check if the CRL has changed
		destFile, err := os.Open(j.Destination)
		if err == nil {
			defer destFile.Close()

			destHash := sha256.New()
			if _, err := io.Copy(destHash, bufio.NewReader(destFile)); err != nil {
				log.Error().Interface("id", j.ID).Str("dest", j.Destination).Str("url", j.URL).Err(err).Msg("failed to compare CRL files")
				j.Metrics.ErrorTotal.Inc()
				j.Metrics.Error.With(prometheus.Labels{"job": fmt.Sprintf("%v", j.ID), "file": j.Destination}).Inc()
				return
			}

			// No changes in the CRL, job is done
			if bytes.Equal(tempHash.Sum(nil), destHash.Sum(nil)) {
				log.Info().Interface("id", j.ID).Str("dest", j.Destination).Str("url", j.URL).Msg("CRL source did not change")
				j.Metrics.SuccessTotal.Inc()
				j.Metrics.Success.With(prometheus.Labels{"job": fmt.Sprintf("%v", j.ID), "file": j.Destination}).Inc()
				return
			}

			// Close the destination file because Windows doesn't like replacing opened files
			destFile.Close()
		}
	}

	if runtime.GOOS != "windows" {
		if err := os.Chown(tempFile.Name(), j.UID, j.GID); err != nil {
			log.Error().Interface("id", j.ID).Str("dest", j.Destination).Str("url", j.URL).Err(err).Msg("temporary file chown failed")
			j.Metrics.ErrorTotal.Inc()
			j.Metrics.Error.With(prometheus.Labels{"job": fmt.Sprintf("%v", j.ID), "file": j.Destination}).Inc()
			return
		}
		if err := os.Chmod(tempFile.Name(), os.FileMode(j.Mode)); err != nil {
			log.Error().Interface("id", j.ID).Str("dest", j.Destination).Str("url", j.URL).Err(err).Msg("temporary file chmod failed")
			j.Metrics.ErrorTotal.Inc()
			j.Metrics.Error.With(prometheus.Labels{"job": fmt.Sprintf("%v", j.ID), "file": j.Destination}).Inc()
			return
		}
	}

	// Replace the destination file atomically
	if err := tempFile.CloseAtomicallyReplace(); err != nil {
		log.Error().Interface("id", j.ID).Str("dest", j.Destination).Str("url", j.URL).Err(err).Msg("failed to replace existing CRL file")
		j.Metrics.ErrorTotal.Inc()
		j.Metrics.Error.With(prometheus.Labels{"job": fmt.Sprintf("%v", j.ID), "file": j.Destination}).Inc()
		return
	}

	log.Info().Interface("id", j.ID).Str("dest", j.Destination).Str("url", j.URL).Msg("updated target CRL file")
	j.Metrics.SuccessTotal.Inc()
	j.Metrics.Success.With(prometheus.Labels{"job": fmt.Sprintf("%v", j.ID), "file": j.Destination}).Inc()
}

func (j *CRLJob) Prepare() error {
	var err error

	// Validate source and destination
	if j.URL == "" || j.Destination == "" {
		return errors.New("empty 'url' and/or 'dest' parameters")
	}

	// Validate owner and group on non-Windows hosts
	if runtime.GOOS != "windows" {
		if j.Owner != "" {
			u, err := user.Lookup(j.Owner)
			if err != nil {
				return errors.Wrap(err, "user lookup failed")
			}
			j.UID, err = strconv.Atoi(u.Uid)
			if err != nil {
				return errors.Wrap(err, "uid conversion failed")
			}
		} else {
			j.UID = os.Getuid()
		}

		if j.Group != "" {
			g, err := user.LookupGroup(j.Group)
			if err != nil {
				return errors.Wrap(err, "group lookup failed")
			}
			j.GID, err = strconv.Atoi(g.Gid)
			if err != nil {
				return errors.Wrap(err, "gid conversion failed")
			}
		} else {
			j.GID = os.Getgid()
		}

		if j.Mode == 0 {
			j.Mode = DefaultFileMode
		}
	}

	// Validate schedule (if not specified/invalid)
	if _, err := cron.ParseStandard(j.Schedule); err != nil {
		j.Schedule = DefaultSchedule
	}
	// Validate download attempt timeout (if not specified/invalid)
	if j.TimeoutDuration, err = time.ParseDuration(j.TimeoutHuman); err != nil {
		j.TimeoutDuration = DefaultTimeoutDuration
	}
	// Validate CRL size limit
	if j.SizeLimit <= 0 {
		j.SizeLimit = DefaultSizeLimit
	}

	return nil
}

// Download CRL file and compute its checksum
func downloadCRL(url string, w io.Writer, h hash.Hash, timeout time.Duration, limit int64, force bool) error {
	c := &http.Client{Timeout: timeout, Transport: &http.Transport{DisableKeepAlives: true, DialContext: (&net.Dialer{KeepAlive: -1}).DialContext}}
	r, err := c.Get(url)
	if r != nil {
		defer r.Body.Close()
	}
	if err != nil {
		return errors.Wrap(err, "http request failed")
	}

	// Destination is the temporary file writer
	// Source is the entire response body
	dest := w
	src := io.Reader(r.Body)

	if !force {
		// Destination is the temporary file and its hash
		dest = io.MultiWriter(w, h)

		// Read a small fragment of the response body first
		head := make([]byte, 24)
		if _, err := io.ReadFull(r.Body, head); err != nil {
			return errors.Wrap(err, "head read failed")
		}

		// Check if we're being offered a CRL file
		if !isCRL(head) {
			return errors.New("source is not a DER or PEM encoded CRL")
		}

		// Source is the header and the remainder of the response body
		src = utils.LimitStrictReader(io.MultiReader(bytes.NewReader(head), src), limit)
	}

	// Copy source to destination and flush the temporary file writer
	if _, err = io.Copy(dest, src); err != nil {
		return errors.Wrap(err, "copy failed")
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
	zerolog.TimeFieldFormat = time.RFC3339
	log.Logger = zerolog.New(os.Stdout).With().Timestamp().Logger()

	// cmd-line arguments
	cfgPath := flag.String("cfg", "/etc/crl-updater.yaml", "path to a config file in YAML format")
	metricsAddr := flag.String("metrics", ":8080", "address for publishing metrics in Prometheus format")
	flag.Parse()

	cfgFile, err := os.Open(*cfgPath)
	if err != nil {
		log.Fatal().Err(err).Msg("config file opening failed")
	}

	// Unmarshal config file
	cfg, err := makeConfig(bufio.NewReader(cfgFile))
	if err != nil {
		log.Fatal().Err(err).Msg("config file parsing failed")
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
		// Prepare and validate job parameters
		if err := job.Prepare(); err != nil {
			log.Error().Str("dest", job.Destination).Str("url", job.URL).Err(err).Msg("skipping job")
			continue
		}

		// Add job to scheduler
		id, err := sched.AddJob(job.Schedule, job)
		if err != nil {
			log.Error().Str("dest", job.Destination).Str("url", job.URL).Err(err).Msg("failed to add CRL update job")
			continue
		}
		job.ID = id
		job.Metrics = pmMetrics
		log.Info().Interface("id", job.ID).Str("dest", job.Destination).Str("url", job.URL).Msg("added CRL update job")
	}
	// Run jobs
	sched.Start()

	// Serve metrics
	http.Handle("/metrics", promhttp.HandlerFor(pmReg, promhttp.HandlerOpts{}))
	if err := http.ListenAndServe(*metricsAddr, nil); err != nil {
		log.Fatal().Err(err).Msg("listen failed")
	}
}
