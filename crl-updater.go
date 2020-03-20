package main

import (
	"bufio"
	"flag"
	"io/ioutil"
	"log"
	"os"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

type CRLJob struct {
	Source      string `yaml:"src"`
	Destination string `yaml:"dest"`
	Schedule    string `yaml:"schedule"`
}

func (j *CRLJob) Run() {
	log.Printf("%s -> %s", j.Source, j.Destination)
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

func main() {
	cfgPath := flag.String("cfg", "/etc/crl-updater.yaml", "path to a config file in YAML format")
	flag.Parse()

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
		if _, err := cron.ParseStandard(job.Schedule); err != nil {
			log.Printf("failed to parse job schedule (%s): %v", job.Schedule, err)
			job.Schedule = "* * * * *"
		}
		id, err := sched.AddJob(job.Schedule, job)
		if err != nil {
			log.Printf("failed to add CRL update job: %v", err)
		}
		log.Printf("added CRL update job: [%v] (%s) %s -> %s", id, job.Schedule, job.Source, job.Destination)
	}
	sched.Start()

	for {
		time.Sleep(time.Second)
	}
}
