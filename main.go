package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/insomniacslk/xjson"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

var (
	flagPath              = flag.String("p", "/metrics", "HTTP path where to expose metrics to")
	flagListen            = flag.String("l", ":9101", "Address to listen to")
	flagSpeedTestCLI      = flag.String("s", "speedtest-cli", "Path to speedtest-cli")
	flagSpeedTestServerID = flag.Int("S", 0, "Server ID obtained with `speedtest-cli --list`")
	flagSleepInterval     = flag.Duration("i", 30*time.Minute, "Interval between speedtest executions, expressed as a Go duration string")
	flagRetryInterval     = flag.Duration("r", 1*time.Minute, "Interval between retries when 'speedtest --list' fails to find a server, expressed as a Go duration string")
	flagInsecure          = flag.Bool("I", false, "Insecure mode: use HTTP instead of HTTPS")
	flagDebug             = flag.Bool("d", false, "Enable debugging output")
	flagMaxDistance       = flag.Int("m", 0, "Max distance in km to the speedtest server")
	flagServerRegexp      = flag.String("R", "", "Regular expression to match the candidate servers")
)

var errRetryable403 = fmt.Errorf("speedtest temporarily failed for HTTP 403, try again later")

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

func speedtest(cliPath string, serverIDs []int, insecure bool) (*speedTestResult, error) {
	args := []string{"--json"}
	usingServerIDs := false
	for _, serverID := range serverIDs {
		if serverID != 0 {
			args = append(args, "--server", fmt.Sprintf("%d", serverID))
			usingServerIDs = true
		}
	}
	if !insecure {
		if usingServerIDs {
			logrus.Warningf("Disabling --secure because it is apparently incompatible with a custom list of servers")
		} else {
			args = append(args, "--secure")
		}
	}
	cmd := exec.Command(cliPath, args...)
	var outb, errb bytes.Buffer
	cmd.Stdout = &outb
	cmd.Stderr = &errb
	logrus.Debugf("Executing command %+v", cmd)
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
				return nil, errRetryable403
			}
		}
		if err := scanner.Err(); err != nil {
			logrus.Warning("Text scanner failed: %w", err)
		}
		return nil, fmt.Errorf("failed to execute speedtest CLI: %w\nStdout: %s\nStderr: %s", runErr, outstr, errstr)
	}
	logrus.Debugf("Raw output: %s", outb.String())
	var ret speedTestResult
	if err := json.Unmarshal(outb.Bytes(), &ret); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JSON result: %w", err)
	}
	logrus.Debugf("Speedtest results: %+v", ret)
	return &ret, nil
}

type SpeedtestServer struct {
	ID         int
	Name       string
	DistanceKm int
}

var serverListRegexp = regexp.MustCompile(`(\d+)\) (.+) [[](\d+\.\d+) km[]]`)

func getServers(cliPath string, insecure bool) ([]SpeedtestServer, error) {
	args := []string{"--list"}
	if !insecure {
		args = append(args, "--secure")
	}
	cmd := exec.Command(cliPath, args...)
	var outb, errb bytes.Buffer
	cmd.Stdout = &outb
	cmd.Stderr = &errb
	logrus.Debugf("Executing command %+v", cmd)
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
				return nil, errRetryable403
			}
		}
		if err := scanner.Err(); err != nil {
			logrus.Warning("Text scanner failed: %w", err)
		}
		return nil, fmt.Errorf("failed to get speedtest's closest servers list: %w\nStdout: %s\nStderr: %s", runErr, outstr, errstr)
	}
	scanner := bufio.NewScanner(&outb)
	servers := make([]SpeedtestServer, 0)
	for scanner.Scan() {
		// parse output line. The format is "ServerID) Server Name [123.4 km]
		line := scanner.Text()
		logrus.Debugf("Server list line: %s", line)
		matches := serverListRegexp.FindStringSubmatch(line)
		logrus.Debugf("Matches: %#+v", matches)
		if len(matches) != 4 {
			continue
		}
		serverID, err := strconv.ParseInt(matches[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse integer string %q: %v", matches[1], err)
		}
		distanceKm, err := strconv.ParseFloat(matches[3], 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse integer string %q: %v", matches[3], err)
		}
		servers = append(servers, SpeedtestServer{
			ID:         int(serverID),
			Name:       matches[2],
			DistanceKm: int(distanceKm),
		})
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("no servers found")
	}
	return servers, nil
}

func setError(speedtestSpeedGauge prometheus.GaugeVec, speedtestPingGauge prometheus.Gauge) {
	// update value
	speedtestSpeedGauge.Reset()
	speedtestSpeedGauge.WithLabelValues(
		"upload",
		// client ip, client isp, client country
		"", "", "",
		// server sponsor, server host, server country
		"", "", "",
	).Set(0)
	speedtestSpeedGauge.WithLabelValues(
		"download",
		// client ip, client isp, client country
		"", "", "",
		// server sponsor, server host, server country
		"", "", "",
	).Set(0)
	speedtestPingGauge.Set(0)
}

func main() {
	flag.Parse()
	logrus.SetLevel(logrus.InfoLevel)
	if *flagDebug {
		logrus.SetLevel(logrus.DebugLevel)
	}

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
		logrus.Fatalf("Failed to register speedtest speed gauge: %v", err)
	}
	if err := prometheus.Register(speedtestPingGauge); err != nil {
		logrus.Fatalf("Failed to register speedtest ping gauge: %v", err)
	}
	var serverRegexp *regexp.Regexp
	if *flagServerRegexp != "" {
		rx, err := regexp.Compile(*flagServerRegexp)
		if err != nil {
			logrus.Fatalf("Failed to parse server regexp: %v", err)
		}
		serverRegexp = rx
	}

	go func() {
		for {
			serverIDs := make([]int, 0)
			var (
				res *speedTestResult
				err error
			)
			if *flagServerRegexp == "" && *flagMaxDistance == 0 {
				// run the speedtest without any server preference
				if *flagSpeedTestServerID != 0 {
					logrus.Infof("Using server ID %d", *flagSpeedTestServerID)
					serverIDs = []int{*flagSpeedTestServerID}
				} else {
					logrus.Infof("Using random server")
				}
			} else {
				allServers, err := getServers(*flagSpeedTestCLI, *flagInsecure)
				if err != nil {
					logrus.Warningf("Failed to get list of speedtest servers: %v", err)
					setError(*speedtestSpeedGauge, speedtestPingGauge)
					logrus.Infof("Sleeping %s before retrying to get server list...", *flagRetryInterval)
					time.Sleep(*flagRetryInterval)
					continue
				}
				logrus.Infof("Found %d total servers (before filtering)", len(allServers))
				if serverRegexp != nil {
					// filter servers by regexp first
					logrus.Infof("Filtering servers matching regexp %q", *flagServerRegexp)
					var servers []SpeedtestServer
					for _, s := range allServers {
						if serverRegexp.MatchString(s.Name) {
							servers = append(servers, s)
						}
					}
					logrus.Infof("Remaining servers after regexp filtering: %d", len(servers))
					allServers = servers
				}
				if *flagMaxDistance > 0 {
					logrus.Infof("Filtering servers within %d km", *flagMaxDistance)
					var servers []SpeedtestServer
					for _, s := range allServers {
						if s.DistanceKm <= *flagMaxDistance {
							servers = append(servers, s)
						}
					}
					logrus.Infof("Remaining servers after distance filtering: %d", len(servers))
					allServers = servers
				}
				// now get the list of server IDs from the filtered servers
				for _, s := range allServers {
					serverIDs = append(serverIDs, s.ID)
				}
				if len(serverIDs) == 0 {
					logrus.Warningf("No server found within %d km", *flagMaxDistance)
					setError(*speedtestSpeedGauge, speedtestPingGauge)
					logrus.Infof("Sleeping %s before retrying to get server list...", *flagRetryInterval)
					time.Sleep(*flagRetryInterval)
					continue
				}
				logrus.Infof("Found %d servers after filtering", len(allServers))
				for idx, s := range allServers {
					logrus.Infof("%d) (ID: %d) %s, %d km", idx+1, s.ID, s.Name, s.DistanceKm)
				}
			}
			logrus.Infof("Running speed test with server IDs %v", serverIDs)
			res, err = speedtest(*flagSpeedTestCLI, serverIDs, *flagInsecure)
			if err != nil {
				if err == errRetryable403 {
					logrus.Warningf("Retryable HTTP 403 error, sleeping for %s: %v", defaultRetryInterval, err)
					time.Sleep(defaultRetryInterval)
					continue
				}
				logrus.Warningf("Wailed to run speed test: %v", err)
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
			logrus.Infof("Sleeping %s...", *flagSleepInterval)
			time.Sleep(*flagSleepInterval)
		}
	}()

	http.Handle(*flagPath, promhttp.Handler())
	logrus.Infof("Starting server on %s", *flagListen)
	logrus.Fatal(http.ListenAndServe(*flagListen, nil))
}
