package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

type CRLJob struct {
	ID cron.EntryID

	Source          string `yaml:"src"`
	Destination     string `yaml:"dest"`
	Schedule        string `yaml:"schedule"`
	TimeoutHuman    string `yaml:"timeout"`
	TimeoutDuration time.Duration
}

func (j *CRLJob) Run() {
	tmp, err := os.Create(fmt.Sprintf("%s.tmp", j.Destination))
	if err != nil {
		log.Printf("failed to create a temporary file: %v", err)
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	w := bufio.NewWriter(tmp)
	if err := downloadCRL(j.Source, w, j.TimeoutDuration); err != nil {
		log.Printf("failed to download CRL: %v", err)
		return
	}

	if err := w.Flush(); err != nil {
		log.Printf("failed to write to temporary file: %v", err)
		return
	}

	tmp.Close()
	if err = os.Rename(tmp.Name(), j.Destination); err != nil {
		log.Printf("failed to replace existing CRL file: %v", err)
		return
	}
}

type Config struct {
	CRLJobs []*CRLJob `yaml:"jobs"`
}

func makeConfig(r *bufio.Reader) (*Config, error) {
	b, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, err
	}

	cfg := &Config{}
	err = yaml.Unmarshal(b, cfg)
	if err != nil {
		return nil, err
	}

	return cfg, nil
}

// Download CRL file from the specified location and write it to the specified Writer
func downloadCRL(src string, dest *bufio.Writer, timeout time.Duration) error {
	c := &http.Client{Timeout: timeout}
	r, err := c.Get(src)
	if err != nil {
		return err
	}
	defer r.Body.Close()

	_, err = io.Copy(dest, r.Body)
	if err != nil {
		return err
	}

	return nil
}

func main() {
	// cmd-line arguments
	cfgPath := flag.String("cfg", "/etc/crl-updater.yaml", "path to a config file in YAML format")
	flag.Parse()

	// Config file parsing
	cfgFile, err := os.Open(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}

	cfg, err := makeConfig(bufio.NewReader(cfgFile))
	if err != nil {
		log.Fatalf("failed to parse configuration: %v", err)
	}
	cfgFile.Close()

	sched := cron.New()
	jobs := cfg.CRLJobs
	for _, job := range jobs {
		if job.Source == "" || job.Destination == "" {
			log.Printf("empty source or destination, skipping job: (%s) %s -> %s", job.Schedule, job.Source, job.Destination)
			continue
		}
		// Default schedule
		if _, err := cron.ParseStandard(job.Schedule); err != nil {
			log.Printf("failed to parse job schedule (%s): %v", job.Schedule, err)
			job.Schedule = "@hourly"
		}
		// Default timeout
		if job.TimeoutDuration, err = time.ParseDuration(job.TimeoutHuman); err != nil {
			log.Printf("failed to parse job timeout (%s): %v", job.TimeoutHuman, err)
			job.TimeoutDuration = time.Second * 60
		}
		// Add job to scheduler
		id, err := sched.AddJob(job.Schedule, job)
		if err != nil {
			log.Printf("failed to add CRL update job: %v", err)
			continue
		}
		// Store job ID
		job.ID = id
		log.Printf("added CRL update job: [%v] (%s) %s -> %s", job.ID, job.Schedule, job.Source, job.Destination)
	}
	sched.Start()

	for {
		time.Sleep(time.Second)
	}
}
