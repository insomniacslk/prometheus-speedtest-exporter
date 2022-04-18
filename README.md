# prometheus-speedtest-exporter

This is a speedtest exporter for Prometheus. It uses the [`speedtest` CLI](https://www.speedtest.net/apps/cli).

It will export two metrics:
* `speedtest_speed_bits_per_second`, with a `direction` field that can be either "upload" or "download"
* `speedtest_ping_msec`

## Run it

```
go build
./prometheus-speedtest-exporter
```

## Grafana

See dashboard at
[dashboard.json](https://github.com/insomniacslk/prometheus-weather-exporter/blob/main/dashboard.json)
