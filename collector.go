package exporter

import (
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	fcgiclient "github.com/tomasen/fcgi_client"
	"go.uber.org/zap"
)

var (
	statusLineRegexp = regexp.MustCompile(`(?m)^(.*):\s+(.*)$`)
)

type collector struct {
	exporter           *Exporter
	up                 *prometheus.Desc
	acceptedConn       *prometheus.Desc
	listenQueue        *prometheus.Desc
	maxListenQueue     *prometheus.Desc
	listenQueueLength  *prometheus.Desc
	phpProcesses       *prometheus.Desc
	maxActiveProcesses *prometheus.Desc
	maxChildrenReached *prometheus.Desc
	slowRequests       *prometheus.Desc
	scrapeFailures     *prometheus.Desc
	failureCount       int

	oldAcceptedConn       *prometheus.Desc
	oldListenQueue        *prometheus.Desc
	oldMaxListenQueue     *prometheus.Desc
	oldListenQueueLength  *prometheus.Desc
	oldIdleProcesses      *prometheus.Desc
	oldActiveProcesses    *prometheus.Desc
	oldTotalProcesses     *prometheus.Desc
	oldMaxActiveProcesses *prometheus.Desc
	oldMaxChildrenReached *prometheus.Desc
	oldSlowRequests       *prometheus.Desc
	oldScrapeFailures     *prometheus.Desc
}

const metricsNamespace = "phpfpm"

func newFuncMetric(metricName string, docString string, labels []string) *prometheus.Desc {
	return prometheus.NewDesc(
		prometheus.BuildFQName(metricsNamespace, "", metricName),
		docString, labels, nil,
	)
}

func (e *Exporter) newCollector() *collector {
	return &collector{
		exporter:           e,
		up:                 newFuncMetric("up", "able to contact php-fpm", nil),
		acceptedConn:       newFuncMetric("accepted_connections_total", "Total number of accepted connections", nil),
		listenQueue:        newFuncMetric("listen_queue_connections", "Number of connections that have been initiated but not yet accepted", nil),
		maxListenQueue:     newFuncMetric("listen_queue_max_connections", "Max number of connections the listen queue has reached since FPM start", nil),
		listenQueueLength:  newFuncMetric("listen_queue_length_connections", "The length of the socket queue, dictating maximum number of pending connections", nil),
		phpProcesses:       newFuncMetric("processes_total", "process count", []string{"state"}),
		maxActiveProcesses: newFuncMetric("active_max_processes", "Maximum active process count", nil),
		maxChildrenReached: newFuncMetric("max_children_reached_total", "Number of times the process limit has been reached", nil),
		slowRequests:       newFuncMetric("slow_requests_total", "Number of requests that exceed request_slowlog_timeout", nil),
		scrapeFailures:     newFuncMetric("scrape_failures_total", "Number of errors while scraping php_fpm", nil),

		oldAcceptedConn:       newFuncMetric("accepted_conn", "Total of accepted connections", nil),
		oldListenQueue:        newFuncMetric("listen_queue", "Number of connections that have been initiated but not yet accepted", nil),
		oldMaxListenQueue:     newFuncMetric("max_listen_queue", "Max. connections the listen queue has reached since FPM start", nil),
		oldListenQueueLength:  newFuncMetric("listen_queue_length", "Maximum number of connections that can be queued", nil),
		oldIdleProcesses:      newFuncMetric("idle_processes", "Idle process count", []string{"state"}),
		oldActiveProcesses:    newFuncMetric("active_processes", "Active process count", []string{"state"}),
		oldTotalProcesses:     newFuncMetric("total_processes", "Total process count", nil),
		oldMaxActiveProcesses: newFuncMetric("max_active_processes", "Maximum active process count", nil),
		oldMaxChildrenReached: newFuncMetric("max_children_reached", "Number of times the process limit has been reached", nil),
		oldSlowRequests:       newFuncMetric("slow_requests", "Number of requests that exceed request_slowlog_timeout", nil),
		oldScrapeFailures:     newFuncMetric("scrape_failures", "Number of errors while scraping php_fpm", nil),
	}
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.up
	ch <- c.scrapeFailures
	ch <- c.acceptedConn
	ch <- c.listenQueue
	ch <- c.maxListenQueue
	ch <- c.listenQueueLength
	ch <- c.phpProcesses
	ch <- c.maxActiveProcesses
	ch <- c.maxChildrenReached
	ch <- c.slowRequests

	ch <- c.oldAcceptedConn
	ch <- c.oldListenQueue
	ch <- c.oldMaxListenQueue
	ch <- c.oldListenQueueLength
	ch <- c.oldIdleProcesses
	ch <- c.oldActiveProcesses
	ch <- c.oldTotalProcesses
	ch <- c.oldMaxActiveProcesses
	ch <- c.oldMaxChildrenReached
	ch <- c.oldSlowRequests
	ch <- c.oldScrapeFailures
}

func getDataFastcgi(u *url.URL, timeout time.Duration) ([]byte, error) {
	path := u.Path
	if path == "" {
		path = "/status"
	}

	env := map[string]string{
		"SCRIPT_FILENAME": path,
		"SCRIPT_NAME":     path,
	}

	fcgi, err := fcgiclient.DialTimeout(u.Scheme, u.Host, timeout)
	if err != nil {
		return nil, errors.Wrap(err, "fastcgi dial failed")
	}

	defer fcgi.Close()

	resp, err := fcgi.Get(env)
	if err != nil {
		return nil, errors.Wrap(err, "fastcgi get failed")
	}

	defer resp.Body.Close()

	if resp.StatusCode != 200 && resp.StatusCode != 0 {
		return nil, errors.Errorf("unexpected status: %d", resp.StatusCode)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read fastcgi body")
	}

	return body, nil
}

func getDataHTTP(u *url.URL) ([]byte, error) {
	req := http.Request{
		Method:     "GET",
		URL:        u,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
		Host:       u.Host,
	}

	resp, err := http.DefaultClient.Do(&req)
	if err != nil {
		return nil, errors.Wrap(err, "HTTP request failed")
	}

	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, errors.Errorf("unexpected HTTP status: %d", resp.StatusCode)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read http body")
	}

	return body, nil
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	up := 1.0
	var (
		body []byte
		err  error
	)

	if c.exporter.fcgiEndpoint != nil {
		body, err = getDataFastcgi(c.exporter.fcgiEndpoint, c.exporter.fcgiTimeout)
	} else {
		body, err = getDataHTTP(c.exporter.endpoint)
	}

	if err != nil {
		up = 0.0
		c.exporter.logger.Error("failed to get php-fpm status", zap.Error(err))
		c.failureCount++
	}
	ch <- prometheus.MustNewConstMetric(
		c.up,
		prometheus.GaugeValue,
		up,
	)

	ch <- prometheus.MustNewConstMetric(
		c.scrapeFailures,
		prometheus.CounterValue,
		float64(c.failureCount),
	)

	// dial timeout
	if err, ok := err.(net.Error); ok && err.Timeout() {
		ch <- prometheus.MustNewConstMetric(
			c.oldActiveProcesses,
			prometheus.GaugeValue,
			1000.0,
			"active",
		)
	}

	if up == 0.0 {
		return
	}

	matches := statusLineRegexp.FindAllStringSubmatch(string(body), -1)
	for _, match := range matches {
		key := match[1]
		value, err := strconv.Atoi(match[2])
		if err != nil {
			continue
		}

		var desc *prometheus.Desc
		var odesc *prometheus.Desc
		var valueType prometheus.ValueType
		labels := []string{}

		switch key {
		case "accepted conn":
			desc = c.acceptedConn
			odesc = c.oldAcceptedConn
			valueType = prometheus.CounterValue
		case "listen queue":
			desc = c.listenQueue
			odesc = c.oldListenQueue
			valueType = prometheus.GaugeValue
		case "max listen queue":
			desc = c.maxListenQueue
			odesc = c.oldMaxListenQueue
			valueType = prometheus.CounterValue
		case "listen queue len":
			desc = c.listenQueueLength
			odesc = c.oldListenQueueLength
			valueType = prometheus.GaugeValue
		case "idle processes":
			desc = c.phpProcesses
			odesc = c.oldIdleProcesses
			valueType = prometheus.GaugeValue
			labels = append(labels, "idle")
		case "active processes":
			desc = c.phpProcesses
			odesc = c.oldActiveProcesses
			valueType = prometheus.GaugeValue
			labels = append(labels, "active")
		case "max active processes":
			desc = c.maxActiveProcesses
			odesc = c.oldMaxActiveProcesses
			valueType = prometheus.CounterValue
		case "max children reached":
			desc = c.maxChildrenReached
			odesc = c.oldMaxChildrenReached
			valueType = prometheus.CounterValue
		case "slow requests":
			desc = c.slowRequests
			odesc = c.oldSlowRequests
			valueType = prometheus.CounterValue
		case "total processes":
			odesc = c.oldTotalProcesses
			valueType = prometheus.GaugeValue
		default:
			continue
		}

		if desc != nil {
			m, err := prometheus.NewConstMetric(desc, valueType, float64(value), labels...)
			if err != nil {
				c.exporter.logger.Error(
					"failed to create metrics",
					zap.String("key", key),
					zap.Error(err),
				)
				continue
			}

			ch <- m
		}

		if odesc != nil {
			m, err := prometheus.NewConstMetric(odesc, valueType, float64(value), labels...)
			if err != nil {
				c.exporter.logger.Error(
					"failed to create old metrics",
					zap.String("key", key),
					zap.Error(err),
				)
				continue
			}

			ch <- m
		}

	}
}
