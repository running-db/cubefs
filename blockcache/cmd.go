// Copyright 2022 The Chubao Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package main

import (
	"flag"
	"fmt"
	"github.com/cubefs/cubefs/util/stat"
	"golang.org/x/net/context"
	syslog "log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/cubefs/cubefs/blockcache/bcache"
	"github.com/cubefs/cubefs/cmd/common"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/config"
	"github.com/cubefs/cubefs/util/errors"
	"github.com/cubefs/cubefs/util/log"
	sysutil "github.com/cubefs/cubefs/util/sys"
	"github.com/cubefs/cubefs/util/ump"
	"github.com/jacobsa/daemonize"
)

const (
	ConfigKeyRole       = "role"
	ConfigKeyLogDir     = "logDir"
	ConfigKeyLogLevel   = "logLevel"
	ConfigKeyProfPort   = "prof"
	ConfigKeyWarnLogDir = "warnLogDir"
)

const (
	UnixSocketPath    = "/var/lib/adls/bcache.socket"
	DefaultTimeOut    = 5 * time.Second
	UnixSocketStopUrl = "http://unix/stop"
)

const (
	RoleBcache = "blockcache"
)

const (
	LoggerOutput = "output.log"
)

var (
	configFile       = flag.String("c", "", "config file path")
	configVersion    = flag.Bool("v", false, "show version")
	configForeground = flag.Bool("f", false, "run foreground")
)

func interceptSignal(s common.Server) {
	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGINT, syscall.SIGTERM)
	syslog.Println("action[interceptSignal] register system signal.")
	go func() {
		sig := <-sigC
		syslog.Printf("action[interceptSignal] received signal: %s.", sig.String())
		s.Shutdown()
	}()
}

func modifyOpenFiles() (err error) {
	var rLimit syscall.Rlimit
	err = syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit)
	if err != nil {
		return fmt.Errorf("Error Getting Rlimit %v", err.Error())
	}
	syslog.Println(rLimit)
	rLimit.Max = 1024000
	rLimit.Cur = 1024000
	err = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rLimit)
	if err != nil {
		return fmt.Errorf("Error Setting Rlimit %v", err.Error())
	}
	err = syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit)
	if err != nil {
		return fmt.Errorf("Error Getting Rlimit %v", err.Error())
	}
	syslog.Println("Rlimit Final", rLimit)
	return
}

func main() {

	if os.Args[1] == "stop" {
		stopService()
		os.Exit(0)
	}

	flag.Parse()

	Version := proto.DumpVersion("Server")
	if *configVersion {
		fmt.Printf("%v", Version)
		os.Exit(0)
	}

	/*
	 * LoadConfigFile should be checked before start daemon, since it will
	 * call os.Exit() w/o notifying the parent process.
	 */
	cfg, err := config.LoadConfigFile(*configFile)
	if err != nil {
		daemonize.SignalOutcome(err)
		os.Exit(1)
	}

	if !*configForeground {
		if err := startDaemon(); err != nil {
			fmt.Printf("Server start failed: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	/*
	 * We are in daemon from here.
	 * Must notify the parent process through SignalOutcome anyway.
	 */

	role := RoleBcache
	logDir := cfg.GetString(ConfigKeyLogDir)
	logLevel := cfg.GetString(ConfigKeyLogLevel)
	profPort := cfg.GetString(ConfigKeyProfPort)
	umpDatadir := cfg.GetString(ConfigKeyWarnLogDir)

	// Init server instance with specified role configuration.
	var (
		server common.Server
		module string
	)
	switch role {
	case RoleBcache:
		server = bcache.NewServer()
		module = RoleBcache
	default:
		err = errors.NewErrorf("Fatal: role mismatch: %s", role)
		fmt.Println(err)
		daemonize.SignalOutcome(err)
		os.Exit(1)
	}

	// Init logging
	var (
		level log.Level
	)
	switch strings.ToLower(logLevel) {
	case "debug":
		level = log.DebugLevel
	case "info":
		level = log.InfoLevel
	case "warn":
		level = log.WarnLevel
	case "error":
		level = log.ErrorLevel
	default:
		level = log.ErrorLevel
	}

	_, err = log.InitLog(logDir, module, level, nil)
	if err != nil {
		err = errors.NewErrorf("Fatal: failed to init log - %v", err)
		fmt.Println(err)
		daemonize.SignalOutcome(err)
		os.Exit(1)
	}
	defer log.LogFlush()

	// Init output file
	outputFilePath := path.Join(logDir, module, LoggerOutput)
	outputFile, err := os.OpenFile(outputFilePath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0666)
	if err != nil {
		err = errors.NewErrorf("Fatal: failed to open output path - %v", err)
		fmt.Println(err)
		daemonize.SignalOutcome(err)
		os.Exit(1)
	}
	//stat log
	_, err = stat.NewStatistic(logDir, "blockcache", int64(stat.DefaultStatLogSize),
		stat.DefaultTimeOutUs, true)
	if err != nil {
		err = errors.NewErrorf("Init stat log fail: %v\n", err)
		fmt.Println(err)
		daemonize.SignalOutcome(err)
		os.Exit(1)
	}
	stat.ClearStat()

	defer func() {
		outputFile.Sync()
		outputFile.Close()
	}()
	syslog.SetOutput(outputFile)

	if err = sysutil.RedirectFD(int(outputFile.Fd()), int(os.Stderr.Fd())); err != nil {
		err = errors.NewErrorf("Fatal: failed to redirect fd - %v", err)
		syslog.Println(err)
		daemonize.SignalOutcome(err)
		os.Exit(1)
	}

	syslog.Printf("Hello, ChubaoFS Storage\n%s\n", Version)

	err = modifyOpenFiles()
	if err != nil {
		err = errors.NewErrorf("Fatal: failed to modify open files - %v", err)
		syslog.Println(err)
		daemonize.SignalOutcome(err)
		os.Exit(1)
	}

	//for multi-cpu scheduling
	runtime.GOMAXPROCS(runtime.NumCPU())
	if err = ump.InitUmp(role, umpDatadir); err != nil {
		log.LogFlush()
		err = errors.NewErrorf("Fatal: failed to init ump warnLogDir - %v", err)
		syslog.Println(err)
		daemonize.SignalOutcome(err)
		os.Exit(1)
	}

	if profPort != "" {
		go func() {
			http.HandleFunc(log.SetLogLevelPath, log.SetLogLevel)
			e := http.ListenAndServe(fmt.Sprintf(":%v", profPort), nil)
			if e != nil {
				log.LogFlush()
				err = errors.NewErrorf("cannot listen pprof %v err %v", profPort, err)
				syslog.Println(err)
				daemonize.SignalOutcome(err)
				os.Exit(1)
			}
		}()
	}

	interceptSignal(server)
	err = server.Start(cfg)
	if err != nil {
		log.LogFlush()
		err = errors.NewErrorf("Fatal: failed to start the ChubaoFS %s daemon err %v - ", role, err)
		syslog.Println(err)
		daemonize.SignalOutcome(err)
		os.Exit(1)
	}

	daemonize.SignalOutcome(nil)

	// Block main goroutine until server shutdown.
	server.Sync()
	log.LogFlush()
	os.Exit(0)
}

func startDaemon() error {
	cmdPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("startDaemon failed: cannot get absolute command path, err(%v)", err)
	}

	configPath, err := filepath.Abs(*configFile)
	if err != nil {
		return fmt.Errorf("startDaemon failed: cannot get absolute command path of config file(%v) , err(%v)", *configFile, err)
	}

	args := []string{"-f"}
	args = append(args, "-c")
	args = append(args, configPath)

	env := []string{
		fmt.Sprintf("PATH=%s", os.Getenv("PATH")),
	}

	err = daemonize.Run(cmdPath, args, env, os.Stdout)
	if err != nil {
		return fmt.Errorf("startDaemon failed: daemon start failed, cmd(%v) args(%v) env(%v) err(%v)\n", cmdPath, args, env, err)
	}

	return nil
}

func stopService() {
	httpc := http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.DialTimeout("unix", UnixSocketPath, DefaultTimeOut)
			},
			MaxConnsPerHost:     20,
			IdleConnTimeout:     DefaultTimeOut,
			MaxIdleConnsPerHost: 20,
		},
		Timeout: DefaultTimeOut,
	}
	req, err := http.NewRequest(http.MethodHead, UnixSocketStopUrl, nil)
	if err != nil {
		fmt.Println("Stop failed:", err.Error())
		return
	}
	res, err := httpc.Do(req)
	if err != nil {
		if res == nil {
			fmt.Println("Stop failed: Service not started.")
		} else {
			fmt.Println("Stop failed:", err.Error())
		}
	}
}