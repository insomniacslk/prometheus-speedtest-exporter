package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"strings"
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
	flagInsecure      = flag.Bool("I", false, "Insecure mode: use HTTP instead of HTTPS")
)

var errRetryable = fmt.Errorf("speedtest temporarily failed, try again later")

const defaultRetryInterval = 60 * time.Second

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

func speedtest(cliPath string, insecure bool) (*speedTestResult, error) {
	args := []string{"--json"}
	if !insecure {
		args = append(args, "--secure")
	}
	cmd := exec.Command(cliPath, args...)
	var outb, errb bytes.Buffer
	cmd.Stdout = &outb
	cmd.Stderr = &errb
	if runErr := cmd.Run(); runErr != nil {
		var (
			errCode int
			errMsg  string
		)
		errstr := errb.String()
		outstr := outb.String()
		scanner := bufio.NewScanner(&errb)
		for scanner.Scan() {
			n, err := fmt.Fscanf(strings.NewReader(scanner.Text()), "ERROR: HTTP Error %d: %s\n", &errCode, &errMsg)
			if err != nil || n != 2 {
				// not an HTTP error string, ignore
				continue
			}
			// at this point we know there's an HTTP error. If it's 403
			// Forbidden we know something's being updated on the SpeedTest
			// side, so we can wait and retry
			if errCode == 403 {
				return nil, errRetryable
			}
		}
		if err := scanner.Err(); err != nil {
			log.Printf("Warning: scanner failed: %w", err)
		}
		return nil, fmt.Errorf("failed to execute speedtest CLI: %w\nStdout: %s\nStderr: %s", runErr, outstr, errstr)
	}
	var ret speedTestResult
	if err := json.Unmarshal(outb.Bytes(), &ret); err != nil {
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
		[]string{"direction", "client_ip", "client_isp", "client_country", "server_sponsor", "server_host", "server_country"},
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
			res, err := speedtest(*flagSpeedTestCLI, *flagInsecure)
			if err != nil {
				if err == errRetryable {
					log.Printf("Retryable error, sleeping for %s", defaultRetryInterval)
					time.Sleep(defaultRetryInterval)
					continue
				}
				log.Printf("ERROR: failed to run speed test: %v", err)
			} else {
				// update value
				speedtestSpeedGauge.Reset()
				speedtestSpeedGauge.WithLabelValues(
					"upload",
					res.Client.IP.String(), res.Client.ISP, res.Client.Country,
					res.Server.Sponsor, res.Server.Host, res.Server.Country,
				).Set(res.Upload)
				speedtestSpeedGauge.WithLabelValues(
					"download",
					res.Client.IP.String(), res.Client.ISP, res.Client.Country,
					res.Server.Sponsor, res.Server.Host, res.Server.Country,
				).Set(res.Download)
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
