package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"time"

	"github.com/insomniacslk/xjson"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	flagPath          = flag.String("p", "/metrics", "HTTP path where to expose metrics to")
	flagListen        = flag.String("l", ":9101", "Address to listen to")
	flagSpeedTestCLI  = flag.String("s", "speedtest-cli", "Path to speedtest-cli")
	flagSleepInterval = flag.Duration("i", 30*time.Minute, "Interval between speedtest executions, expressed as a Go duration string")
)

type speedTestResult struct {
	Download      float64
	Upload        float64
	Ping          float64
	Timestamp     time.Time
	BytesSent     uint
	BytesReceived uint
	Client        clientInfo
	Server        serverInfo
}

type clientInfo struct {
	IP        net.IP
	Lat       string
	Lon       string
	ISP       string
	ISPRating string
	Rating    string
	ISPLavg   string
	ISPULavg  string
	LoggedIn  string
	Country   string
}

type serverInfo struct {
	URL     xjson.URL
	Lat     string
	Lon     string
	Name    string
	Country string
	CC      string
	Sponsor string
	ID      string
	Host    string
	D       float64
	Latency float64
}

func speedtest(cliPath string) (*speedTestResult, error) {
	cmd := exec.Command(cliPath, "--json")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to execute speedtest CLI: %w", err)
	}
	var ret speedTestResult
	if err := json.Unmarshal(out.Bytes(), &ret); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JSON result: %w", err)
	}
	return &ret, nil
}

func main() {
	flag.Parse()

	speedtestSpeedGauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "speedtest_speed_bits_per_second",
			Help: "SpeedTest.net upload and download speed",
		},
		[]string{"direction"},
	)
	speedtestPingGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "speedtest_ping_msec",
		Help: "SpeedTest.net ping latency in milliseconds",
	})
	if err := prometheus.Register(speedtestSpeedGauge); err != nil {
		log.Fatalf("Failed to register speedtest speed gauge: %v", err)
	}
	if err := prometheus.Register(speedtestPingGauge); err != nil {
		log.Fatalf("Failed to register speedtest ping gauge: %v", err)
	}

	go func() {
		for {
			log.Printf("Running speed test...")
			res, err := speedtest(*flagSpeedTestCLI)
			if err != nil {
				log.Printf("ERROR: failed to run speed test: %v", err)
			} else {
				// update value
				speedtestSpeedGauge.WithLabelValues("upload").Set(res.Upload)
				speedtestSpeedGauge.WithLabelValues("download").Set(res.Download)
				speedtestPingGauge.Set(res.Ping)
			}
			log.Printf("Sleeping %s...", *flagSleepInterval)
			time.Sleep(*flagSleepInterval)
		}
	}()

	http.Handle(*flagPath, promhttp.Handler())
	log.Printf("Starting server on %s", *flagListen)
	log.Fatal(http.ListenAndServe(*flagListen, nil))
}
