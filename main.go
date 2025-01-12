package main

import (
	//"fmt"
	"gopkg.in/alecthomas/kingpin.v2"
	"io"
	stdlog "log"
	"log/syslog"
	"net/http"
	"os"
	"errors"
	"path"
	"path/filepath"
	"strings"
	"strconv"
	"syscall"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promlog"
	"github.com/prometheus/common/promlog/flag"
	"github.com/prometheus/common/version"

	"github.com/hpcloud/tail"
	"github.com/shirou/gopsutil/process"
)

var (
	logPath          = kingpin.Flag("exim.log-path", "Path to the Exim log files.").Default("/var/log/exim4").Envar("EXIM_LOG_PATH").String()
	mainlog          = kingpin.Flag("exim.mainlog", "Exim main log filename in log-path directory.").Default("mainlog").Envar("EXIM_MAINLOG").String()
	rejectlog        = kingpin.Flag("exim.rejectlog", "Exim reject log filename in log-path directory.").Default("rejectlog").Envar("EXIM_REJECTLOG").String()
	paniclog         = kingpin.Flag("exim.paniclog", "Exim panic log filename in log-path directory.").Default("paniclog").Envar("EXIM_PANICLOG").String()
	eximExec         = kingpin.Flag("exim.executable", "Path name of the Exim daemon executable.").Default("exim4").Envar("EXIM_EXECUTABLE").String()
	useJournal       = kingpin.Flag("exim.use-journal", "Use the systemd journal instead of log file tailing").Envar("EXIM_USE_JOURNAL").Bool()
	syslogIdentifier = kingpin.Flag("exim.syslog-identifier", "Syslog identifier used by Exim").Default("exim").Envar("EXIM_SYSLOG_IDENTIFIER").String()
	inputPath        = kingpin.Flag("exim.input-path", "Path to Exim queue directory.").Default("/var/spool/exim4/input").Envar("EXIM_QUEUE_DIR").String()
	listenAddress    = kingpin.Flag("web.listen-address", "Address to listen on for web interface and telemetry.").Default(":9636").Envar("WEB_LISTEN_ADDRESS").String()
	metricsPath      = kingpin.Flag("web.telemetry-path", "URI under which to expose metrics.").Default("/metrics").Envar("WEB_TELEMETRY_PATH").String()
	tailPoll         = kingpin.Flag("tail.poll", "Poll logs for changes instead of using inotify.").Envar("TAIL_POLL").Bool()
)

const BASE62 = "0123456789aAbBcCdDeEfFgGhHiIjJkKlLmMnNoOpPqQrRsStTuUvVwWxXyYzZ"

var (
	eximUp = prometheus.NewDesc(
		prometheus.BuildFQName("exim", "", "up"),
		"Whether or not the main exim daemon is running",
		nil, nil,
	)
	eximQueue = prometheus.NewDesc(
		prometheus.BuildFQName("exim", "", "queue"),
		"Number of messages currently in queue",
		nil, nil,
	)
	eximProcesses = prometheus.NewDesc(
		prometheus.BuildFQName("exim", "", "processes"),
		"Number of running exim process broken down by state (delivering, handling, etc)",
		[]string{"state"}, nil,
	)
	eximAddresses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: prometheus.BuildFQName("exim", "", "messages_origin"),
			Help: "Total number of logged addresses broken down by domain",
		},
		[]string{"dir", "domain"},
	)
	eximMessages = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: prometheus.BuildFQName("exim", "", "messages_total"),
			Help: "Total number of logged messages broken down by flag (delivered, deferred, etc)",
		},
		[]string{"flag"},
	)
	eximIssues = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: prometheus.BuildFQName("exim", "", "issues_total"),
			Help: "Total number of logged issues broken down by type (tls, smtp, connection, etc)",
		},
		[]string{"type", "ip"},
	)
	eximReject = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: prometheus.BuildFQName("exim", "", "reject_total"),
			Help: "Total number of logged reject messages",
		},
	)
	eximPanic = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: prometheus.BuildFQName("exim", "", "panic_total"),
			Help: "Total number of logged panic messages",
		},
	)
	queueRuns = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: prometheus.BuildFQName("exim", "", "queue_runs"),
			Help: "Total number of queue runs noted in the logs",
		},
	)
	readErrors = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: prometheus.BuildFQName("exim", "log_read", "errors"),
			Help: "Total number of errors encountered while reading the logs",
		},
	)
)

var processFlags = map[string]string{
	"-Mc":  "delivering",
	"-bd":  "handling",
	"-bdf": "handling",
	"-qG":  "running",
}

type Process struct {
	cmdline []string
	leader  bool
}

// map globals we can override in tests
var (
	getProcesses = func() ([]*Process, error) {
		processes, err := process.Processes()
		if err != nil {
			return nil, err
		}
		result := make([]*Process, 0)
		for _, p := range processes {
			cmdline, err := p.CmdlineSlice()
			if err != nil {
				continue
			}
			pid := int(p.Pid)
			pgid, err := syscall.Getpgid(pid)
			if err != nil {
				continue
			}
			leader := pid == pgid
			result = append(result, &Process{cmdline, leader})
		}
		return result, nil
	}
)

type Exporter struct {
	mainlog   string
	rejectlog string
	paniclog  string
	eximBin   string
	inputPath string
	logger    log.Logger
}

func NewExporter(mainlog string, rejectlog string, paniclog string, eximExec string, inputPath string, logger log.Logger) *Exporter {
	return &Exporter{
		mainlog,
		rejectlog,
		paniclog,
		eximExec,
		inputPath,
		logger,
	}
}

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- eximUp
	ch <- eximQueue
	ch <- eximProcesses
}

func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	states := e.ProcessStates()
	up := float64(0)
	if _, ok := states["daemon"]; ok {
		up = 1
	}
	ch <- prometheus.MustNewConstMetric(eximUp, prometheus.GaugeValue, up)
	for label, value := range states {
		ch <- prometheus.MustNewConstMetric(eximProcesses, prometheus.GaugeValue, value, label)
	}
	queue := e.QueueSize()
	if queue >= 0 {
		ch <- prometheus.MustNewConstMetric(eximQueue, prometheus.GaugeValue, queue)
	}
}

func (e *Exporter) ProcessStates() map[string]float64 {
	level.Debug(e.logger).Log("msg", "Reading process states")
	states := make(map[string]float64)
	processes, err := getProcesses()
	if err != nil {
		level.Error(e.logger).Log("msg", err)
		return states
	}
	for _, p := range processes {
		//level.Debug(e.logger).Log("msg", "process state", "isLeader", p.leader, "cmdline", fmt.Sprintf("%v", p.cmdline))
		if len(p.cmdline) < 1 || path.Base(p.cmdline[0]) != e.eximBin {
			continue
		}
		if len(p.cmdline) < 2 {
			states["other"] += 1
		} else if state, ok := processFlags[p.cmdline[1]]; ok {
			if state == "handling" && p.leader {
				states["daemon"] += 1
			} else {
				states[state] += 1
			}
		} else {
			states["other"] += 1
		}
	}
	return states
}

func (e *Exporter) CountMessages(dirname string) float64 {
	dir, err := os.Open(dirname)
	if err != nil {
		return 0
	}
	messages, err := dir.Readdirnames(-1)
	dir.Close()
	if err != nil {
		return 0
	}
	var count float64
	for _, name := range messages {
		if len(name) == 18 && strings.HasSuffix(name, "-H") {
			count += 1
		}
	}
	return count
}

func (e *Exporter) QueueSize() float64 {
	level.Debug(e.logger).Log("msg", "Reading queue size", "path", e.inputPath)
	count := e.CountMessages(e.inputPath)
	for h := 0; h < len(BASE62); h++ {
		hashPath := filepath.Join(e.inputPath, string(BASE62[h]))
		count += e.CountMessages(hashPath)
	}
	level.Debug(e.logger).Log("msg", "Queue size", "count", count)
	return count
}

func (e *Exporter) Start() {
	if *useJournal {
		go e.TailMainLog(e.JournalTail(*syslogIdentifier, syslog.LOG_INFO))
		go e.TailRejectLog(e.JournalTail(*syslogIdentifier, syslog.LOG_NOTICE))
		go e.TailPanicLog(e.JournalTail(*syslogIdentifier, syslog.LOG_ALERT))
	} else {
		go e.TailMainLog(e.FileTail(e.mainlog))
		go e.TailRejectLog(e.FileTail(e.rejectlog))
		go e.TailPanicLog(e.FileTail(e.paniclog))
	}
}

func (e *Exporter) FileTail(filename string) chan *tail.Line {
	logger := log.NewStdlibAdapter(e.logger)
	level.Debug(e.logger).Log("msg", "Start FileTail()", "filename", filename)

	// Try to open file for read; if it works, close and carry on. If not
	// try to discover if the file exists, for a more useful message.
	// Don't die here - it's possible the file will become available.
	fp, err := os.Open(filename)
	if err == nil {
		fp.Close()
		level.Info(e.logger).Log("msg", "Opening log", "filename", filename)
	} else {
		_, status := os.Stat(filename)
		if errors.Is(status, os.ErrNotExist) {
			level.Error(e.logger).Log("msg", "Log file does not exist", "filename", filename)
		} else {
			level.Error(e.logger).Log("msg", "Unable to open log", "filename", filename, "err", status)
		}
	}

	t, err := tail.TailFile(filename, tail.Config{
		Location: &tail.SeekInfo{Whence: io.SeekStart},
		ReOpen:   true,
		Follow:   true,
		Poll:     *tailPoll,
		Logger:   stdlog.New(logger, "", stdlog.LstdFlags),
	})
	if err != nil {
		level.Error(e.logger).Log("msg", "Unable to open log", "err", err)
		level.Debug(e.logger).Log("msg", "Exit FileTail()", "filename", filename)
		os.Exit(1)
	}
	level.Debug(e.logger).Log("msg", "Return FileTail()", "filename", filename)
	return t.Lines
}

// JournalTail conditionally defined based on the "systemd" build tag.

func (e *Exporter) TailMainLog(lines chan *tail.Line) {
	level.Info(e.logger).Log("msg", "Tail mainlog")
	for line := range lines {
		if line.Err != nil {
			level.Error(e.logger).Log("msg", "Caught error while reading mainlog", "err", line.Err)
			readErrors.Inc()
			continue
		}
		level.Debug(e.logger).Log("file", "mainlog", "msg", line.Text)
		parts := strings.SplitN(line.Text, " ", 14)
		size := len(parts)
		if size < 3 {
			level.Info(e.logger).Log("msg", "Short line reading mainlog", "msg", line.Err)
			continue
		}
		// Handle logs when PID logging is enabled
		//   no pid: "2022-04-12 22:14:21 1neNq9-0008OD-06 Completed"
		//   w/ pid: "2022-04-12 22:14:21 [4646] 1neNq9-0008OD-06 Completed"
		var index int
		if parts[2][0] == '[' {
			index = 4
		} else {
			index = 3
		}
		if size < index+1 {
			level.Info(e.logger).Log("msg", "No index part reading mainlog", "msg", line.Err)
			continue
		}
		var tmp string = ""
		if size > index+1 {
			tmp = parts[index+1]
		}
		level.Debug(e.logger).Log("file", "mainlog", "parts[index-1]", parts[index-1], "parts[index]", parts[index], "parts[index+1]", tmp)
		// Example receive & transmit
		// 2023-01-22 00:30:02 1pJOFF-000A1p-6c <= www-data@ivimey.org H=greyarea-web2.cam.ivimey.org [192.168.32.68] P=esmtps X=TLS1.3:ECDHE_SECP256R1__RSA_PSS_RSAE_SHA256__AES_256_GCM:256 CV=no K S=1395 id=E1pJOFF-000EIw-HE@greyarea-web2.cam.ivimey.org
		// 2023-01-22 00:30:02 1pJOFF-000A1p-6c => ruth <root@ivimey.org> R=localuser_ivimeyorg T=imap_delivery C="250 2.0.0 <ruth@ivimey.org> 0YSHJ4qDzGOclgAAZKbkIA Saved"
		switch parts[index] {
		case "<=":
			if (size > index+1) && (parts[index+1] == "<>") {
				eximMessages.With(prometheus.Labels{"flag": "mailnotice"}).Inc()
				level.Debug(e.logger).Log("file", "mainlog", "Inc:", "mailnotice")
			} else {
				dom := strings.SplitN(parts[index+1], "@", 2)
				if (len(dom) >1) {
					eximAddresses.With(prometheus.Labels{"dir": "in", "domain": dom[1]}).Inc()
				}
				eximMessages.With(prometheus.Labels{"flag": "arrived"}).Inc()
				level.Debug(e.logger).Log("file", "mainlog", "Inc:", "arrived")
			}
		case "(=":
			eximMessages.With(prometheus.Labels{"flag": "fakereject"}).Inc()
			level.Debug(e.logger).Log("file", "mainlog", "Inc:", "fakereject")
		case "=>":
			// E.g. "2023-04-02 22:54:59.679 1pj5f3-000R0a-Ua => ruth <ruth@ivimey.org> R=localuser_ivimeyorg T=imap_delivery S=8584 C="250 2.0.0 <ruth@ivimey.org> +WS7JbP5KWSVlQEAZKbkIA Saved" QT=5s DT=0.051s"
			// E.g. "2023-04-03 06:00:25.256 1pjCIp-000SBw-5r => krr@yahoo.com R=dnslookup T=remote_smtp S=7332 H=mta7.am0.yahoodns.net [67.195.204.77] X=TLS1.3:ECDHE_X25519__RSA_PSS_RSAE_SHA256__AES_128_GCM:128 CV=yes C="250 ok dirdel" QT=2.048s DT=1.928s"
			dom := strings.SplitN(parts[index+1], "@", 2)
			if (len(dom) >1) {
				eximAddresses.With(prometheus.Labels{"dir": "out", "domain": dom[1]}).Inc()
				level.Debug(e.logger).Log("file", "mainlog", "dir", "out", "domain", dom[1])
			}
			eximMessages.With(prometheus.Labels{"flag": "delivered"}).Inc()
			level.Debug(e.logger).Log("file", "mainlog", "Inc:", "delivered")
		case "->":
			// E.g. "2023-04-04 08:36:50.114 1pjbDl-000W3q-E6 -> lekhas78@gmail.com R=dnslookup T=remote_smtp S=2137 H=gmail-smtp-in.l.google.com [172.253.116.26] TFO X=TLS1.3:ECDHE_X25519__ECDSA_SECP256R1_SHA256__AES_256_GCM:256 CV=yes K C="250 2.0.0 OK x2-20020adfdcc2000000b002d60952491dsi7369308wrm.96 - gsmtp" QT=0.650s DT=0.549sp"
			dom := strings.SplitN(parts[index+1], "@", 2)
			if (len(dom) >1) {
				eximAddresses.With(prometheus.Labels{"dir": "out", "domain": dom[1]}).Inc()
				level.Debug(e.logger).Log("file", "mainlog", "dir", "out", "domain", dom[1])
			}
			eximMessages.With(prometheus.Labels{"flag": "additional"}).Inc()
			level.Debug(e.logger).Log("file", "mainlog", "Inc:", "additional")
		case ">>":
			eximMessages.With(prometheus.Labels{"flag": "cutthrough"}).Inc()
			level.Debug(e.logger).Log("file", "mainlog", "Inc:", "cutthrough")
		case "*>":
			eximMessages.With(prometheus.Labels{"flag": "suppressed"}).Inc()
			level.Debug(e.logger).Log("file", "mainlog", "Inc:", "suppressed")
		case "**":
			eximMessages.With(prometheus.Labels{"flag": "failed"}).Inc()
			level.Debug(e.logger).Log("file", "mainlog", "Inc:", "failed")
		case "==":
			// E.g. "2023-04-04 16:11:27.273 1pjiJf-000X3F-3n == del.nolan@richmondfellowship.org.uk R=dnslookup T=remote_smtp defer (-44) H=eu-smtp-inbound-1.mimecast.com [195.130.217.241] DT=0.000s: SMTP error from remote mail server after RCPT TO:<del.nolan@richmondfellowship.org.uk>: 451 Internal resource temporarily unavailable - https://community.mimecast.com/docs/DOC-1369#451 [ws0CZUyZOQGkjzrBbHKwvw.uk186]"
			eximMessages.With(prometheus.Labels{"flag": "deferred"}).Inc()
			level.Debug(e.logger).Log("file", "mainlog", "Inc:", "deferred")
		case "Completed":
			// E.g. "2023-04-02 23:16:15.977 1pj5zj-000R3G-Sf Completed QT=0.088s"
			eximMessages.With(prometheus.Labels{"flag": "completed"}).Inc()
			level.Debug(e.logger).Log("file", "mainlog", "Inc:", "completed")
		case "DKIM:":
			eximMessages.With(prometheus.Labels{"flag": "invalid"}).Inc()
			level.Debug(e.logger).Log("file", "mainlog", "Inc:", "invalid")
		case "SMTP":
			// Eg. "2022-06-01 12:33:20 1nwMTE-00061M-Fv SMTP data timeout (message abandoned) on connection from (mail.bmwturo.com) [203.28.246.235] F=<newsletter@mail.bmwturo.com>"
			eximIssues.With(prometheus.Labels{"type": "smtp", "ip": ""}).Inc()
			level.Debug(e.logger).Log("file", "mainlog", "Inc:", "smtp")
		case "Message":
			level.Debug(e.logger).Log("file", "mainlog", "Skip:", parts[index-1])
		case "removed":
			eximMessages.With(prometheus.Labels{"flag": "removed"}).Inc()
			level.Debug(e.logger).Log("file", "mainlog", "Inc:", "removed")
		case "Added":
			// E.g. "2022-05-29 23:52:35 1nvRlt-0018yn-Nl Added host 178.208.32.61 with HELO 'mailing-auth001.mailprotect.be' to known resenders"
			if (parts[len(parts)-1] == "resenders") {
				eximMessages.With(prometheus.Labels{"flag": "resender"}).Inc()
				level.Debug(e.logger).Log("file", "mainlog", "Inc:", "resender")
				continue
			}
			level.Debug(e.logger).Log("file", "mainlog", "Ignore Added:", parts[index+1])
			continue
		default:
			// These messages have no associated queue id:
			//   "2022-04-12 22:14:21 Start queue run: pid=229876"
			//   "2022-06-01 12:04:00 H=(WIN-CLJ1B0GQ6JP) [45.134.23.236] F=<spameri@tiscali.it> rejected RCPT <spameri@tiscali.it>: relay not permitted"
			//
			// These messages do have an associated queue id:
			//   "2022-06-01 13:48:42 1nwNm8-0006NL-F0 H=(mail.kittycaller.com) [23.171.176.140] F=<caskassetsmail@mail.cushcoins.com> rejected after DATA: Your message scored 22.8 SpamAssassin points. Report follows:"
			//   "2022-06-01 11:00:03 1nwL8m-0005q8-75 H=fyi.udnoz.com (mail.uxflight.com) [89.34.27.82] X=TLS1.2:ECDHE_SECP256R1__RSA_SHA512__AES_256_GCM:256 CV=no F=<newsletter-ruth=ivimey.org@mail.uxflight.com> rejected after DATA: Your message scored 23.8 SpamAssassin points. Report follows:"
			if size > (index+7) {
				Fmark := 0
				for i, p := range(parts) {
					// Minimum is "F=<>"
					if len(p) > 3 && p[0:3] == "F=<" {
						Fmark = i
						break
					}
				}
				// Fmark is index of F=<...> line, so after complicating elements!
				level.Debug(e.logger).Log("file", "mainlog", "Fmark", Fmark, "parts[Fmark]", parts[Fmark])
				if Fmark > 0 {
					if (Fmark+1 < len(parts)) && (parts[Fmark+1] == "rejected") {
						if (Fmark+4 < len(parts)) && (parts[Fmark+4] == "relay") {
							eximIssues.With(prometheus.Labels{"type": "relay", "ip": ""}).Inc()
							level.Debug(e.logger).Log("file", "mainlog", "Inc:", "relay")
							continue
						}
						if (Fmark+5 < len(parts)) && (parts[Fmark+3] == "DATA:") && (parts[Fmark+5] == "message") {
							eximIssues.With(prometheus.Labels{"type": "spam", "ip": ""}).Inc()
							level.Debug(e.logger).Log("file", "mainlog", "Inc:", "spam")
							continue
						}
					}
				}
			}
			switch parts[index-1] {
			case "SMTP":
				// Eg "SMTP protocol synchronisation error ..."
				// Eg "SMTP data timeout ..."
				// Eg "SMTP call from [ip] dropped ..."
				eximIssues.With(prometheus.Labels{"type": "smtp", "ip": ""}).Inc()
				level.Debug(e.logger).Log("file", "mainlog", "Inc:", "smtp")
			case "rejected":
				// Eg "rejected EHLO from [ip]: syntactically invalid argument(s): []"
				ip := strings.Trim(strings.TrimSuffix(parts[index+2], ":"), "[]")
				eximIssues.With(prometheus.Labels{"type": "smtp", "ip": ip}).Inc()
				level.Debug(e.logger).Log("file", "mainlog", "Inc:", "smtp")
			case "auth_login":
				// Eg "auth_login authenticator failed for ..."
				ip := strings.Trim(strings.TrimSuffix(parts[index+4], ":"), "[]")
				eximIssues.With(prometheus.Labels{"type": "auth", "ip": ip}).Inc()
				level.Debug(e.logger).Log("file", "mainlog", "Inc:", "auth")
			case "no":
				// Eg "no host name found for IP ..."
				ip := strings.Trim(strings.TrimSuffix(parts[index+6], ":"), "[]")
				eximIssues.With(prometheus.Labels{"type": "hostip", "ip": ip}).Inc()
				level.Debug(e.logger).Log("file", "mainlog", "Inc:", "hostip")
			case "TLS":
				// Eg "TLS error on connection ..."
				// Eg "TLS session (gnutls_handshake): Key usage ..."
				eximIssues.With(prometheus.Labels{"type": "tls", "ip": ""}).Inc()
				level.Debug(e.logger).Log("file", "mainlog", "Inc:", "tls")
			case "Connection":
				// Eg "Connection from [ip] refused: too ...
				ip := strings.Trim(strings.TrimSuffix(parts[index+1], ":"), "[]")
				eximIssues.With(prometheus.Labels{"type": "connect", "ip": ip}).Inc()
				level.Debug(e.logger).Log("file", "mainlog", "Inc:", "connect")
			case "unexpected":
				// Eg "unexpected disconnection ..."
				eximIssues.With(prometheus.Labels{"type": "connect", "ip": ""}).Inc()
				level.Debug(e.logger).Log("file", "mainlog", "Inc:", "connect")
			case "Start":
				// Eg "Start queue run: pid=1234", also "End..."
				queueRuns.Inc()
				level.Debug(e.logger).Log("file", "mainlog", "Inc:", "queue")
			case "End":
				level.Debug(e.logger).Log("file", "mainlog", "Skip:", parts[index-1])
			default:
				level.Debug(e.logger).Log("file", "mainlog", "Ignore:", parts[index-1])
			}
		}
	}
}

func (e *Exporter) TailRejectLog(lines chan *tail.Line) {
	level.Info(e.logger).Log("msg", "Tail rejectlog")
	var lineNumber int
	var inMessage bool
	var newMessage bool
	lineNumber = 0
	inMessage = false

	for line := range lines {
		var index int
		lineNumber++

		if line.Err != nil {
			level.Error(e.logger).Log("msg", "Caught error while reading rejectlog", "line", lineNumber, "err", line.Err)
			readErrors.Inc()
			continue
		}

		// 2022-04-12 22:14:21...
		if line.Text[0] == '2' && line.Text[1] == '0' {
			// Date lines indicate a new message entry.
			inMessage = false
			eximReject.Inc()
		} else if line.Text[0] == ' ' || line.Text[0] == '\t' || line.Text[0] == 'E' || line.Text[0] == 'P' || line.Text[0] == 'I' || line.Text[0] == 'F' || line.Text[0] == 'T' {
			// In-Message lines start with WS, or one of "EPIFT".
			inMessage = true
		}

		level.Debug(e.logger).Log("file", "rejectlog", "newMess", newMessage, "inMess", inMessage, "msg", line.Text)

		if ! inMessage {
			// ... "Your message scored 34.9 SpamAssassin points." ...
			index = strings.Index(line.Text, "SpamAssassin points.")
			if index > 0 {
				var points float64
				var endIdx int

				// Skip back to last digit of number:
				index--
				endIdx = index
				index--
				// Now back to space before the start of number
				for line.Text[index] != ' ' {
					index--
				}
				// Now [index:endIdx] should be something like ' 33.1'
				points, _ = strconv.ParseFloat(strings.Trim(line.Text[index:endIdx], " "), 64)
				level.Info(e.logger).Log("msg", "SpamAssassin points", "points", points, "line", lineNumber, "msg", line.Err)
			}
		}
	}
}

func (e *Exporter) TailPanicLog(lines chan *tail.Line) {
	level.Info(e.logger).Log("msg", "Tail paniclog")
	for line := range lines {
		if line.Err != nil {
			level.Error(e.logger).Log("msg", "Caught error while reading paniclog", "err", line.Err)
			readErrors.Inc()
			continue
		}
		level.Debug(e.logger).Log("file", "paniclog", "msg", line.Text)
		eximPanic.Inc()
	}
}

func init() {
	prometheus.MustRegister(version.NewCollector("exim_exporter"))
	prometheus.MustRegister(eximAddresses)
	prometheus.MustRegister(eximMessages)
	prometheus.MustRegister(eximIssues)
	prometheus.MustRegister(eximReject)
	prometheus.MustRegister(eximPanic)
	prometheus.MustRegister(queueRuns)
	prometheus.MustRegister(readErrors)
}

func main() {
	promlogConfig := &promlog.Config{}
	flag.AddFlags(kingpin.CommandLine, promlogConfig)
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()
	logger := promlog.New(promlogConfig)

	level.Info(logger).Log("msg", "Starting exim exporter", "version", version.Info())
	level.Info(logger).Log("msg", "Build context", "context", version.BuildContext())

	if !path.IsAbs(*mainlog) {
		*mainlog = path.Join(*logPath, *mainlog)
	}
	if !path.IsAbs(*rejectlog) {
		*rejectlog = path.Join(*logPath, *rejectlog)
	}
	if !path.IsAbs(*paniclog) {
		*paniclog = path.Join(*logPath, *paniclog)
	}

	exporter := NewExporter(
		*mainlog,
		*rejectlog,
		*paniclog,
		*eximExec,
		*inputPath,
		logger,
	)
	exporter.QueueSize()
	exporter.Start()
	prometheus.MustRegister(exporter)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write([]byte(`<html>
<head><title>Exim Exporter</title></head>
<body>
  <h1>Exim Exporter</h1>
  <p>` + version.Info() + `</p>
  <p><a href='` + *metricsPath + `'>Metrics</a></p>
</body>
</html>`))
		if err != nil {
			_ = level.Error(logger).Log("msg", err)
		}
	})
	http.Handle(*metricsPath, promhttp.Handler())
	level.Info(logger).Log("msg", "Listening", "address", listenAddress)
	level.Error(logger).Log("msg", "ListenAndServe exited", "err", http.ListenAndServe(*listenAddress, nil))
}
