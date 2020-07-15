# crl-updater
[![FOSSA Status](https://app.fossa.com/api/projects/git%2Bgithub.com%2FNeonSludge%2Fcrl-updater.svg?type=shield)](https://app.fossa.com/projects/git%2Bgithub.com%2FNeonSludge%2Fcrl-updater?ref=badge_shield)


Small daemon that takes a list of CRL file update jobs, runs them periodically and publishes success and failure counts as Prometheus metrics. 

It performs basic sanity checks on the files that are being downloaded:
* Source file MUST be smaller than the limit specified at the job level.
* Source file MUST start with a standard X.509 CRL PEM header (`-----BEGIN X509 CRL-----`) OR its first two bytes MUST equal `0x30 0x82` OR `0x30 0x83`. This check is performed on the first 24 bytes of the source file. If it does not pass, the download attempt will fail.

There is also an option to disable these checks for a specific job.

```
$ crl-updater -h
Usage of crl-updater:
  -cfg string
        path to a config file in YAML format (default "/etc/crl-updater.yaml")
  -metrics string
        address for publishing metrics in Prometheus format (default ":8080")
```

### Config file

#### Example:
```
jobs:
  - url: "http://example.com/crl/crl1.crl"
    dest: /etc/crl/crl1.crl
    schedule: "*/5 * * * *"
    mode: 0600
    owner: user1
    group: group1
    limit: 65536
  - url: "http://example.com/crl/crl2.crl"
    dest: /etc/crl/crl2.crl
    schedule: "0 12 * * *"
    force: true
```

#### Job parameters:

| Parameter | Description                              | Default       |
| --------- | ---------------------------------------- | ------------- |
| url       | URL to download the CRL file from.       | none          |
| dest      | Path to save the downloaded CRL file to. | none          |
| schedule  | CRL update cron schedule.                | @hourly       |
| mode      | Permissions for the destination file.    | 0644          |
| owner     | Owner of the destination file.           | current user  |
| group     | Group of the destination file.           | current group |
| limit     | CRL file size limit in bytes.            | 10485760      |
| force     | Force CRL update, skip all checks.       | false         |

### Metrics

The following Prometheus metrics are available at the `/metrics` endpoint:

| Metric | Description | Type |
| ------ | ----------- | ---- |
crl_updater_success{job="job ID", file="destination file name"} | Number of successful CRL update attempts per job. | Counter |
crl_updater_error{job="job ID", file="destination file name"} | Number of unsuccessful CRL update attempts per job. | Counter |
crl_updater_success_total | Number of successful CRL update attempts. | Counter |
crl_updater_error_total | Number of unsuccessful CRL update attempts. | Counter |


## License
[![FOSSA Status](https://app.fossa.com/api/projects/git%2Bgithub.com%2FNeonSludge%2Fcrl-updater.svg?type=large)](https://app.fossa.com/projects/git%2Bgithub.com%2FNeonSludge%2Fcrl-updater?ref=badge_large)