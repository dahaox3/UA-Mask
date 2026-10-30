package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	"gopkg.in/natefinch/lumberjack.v2"
)

var (
	version = "0.4.3+ttlboot3"
)

func setupLogging(logLevel, logFile string) {
	if logFile != "" {
		// 如果指定了 -log 文件路径，则使用 lumberjack 进行文件滚动日志
		logFileRotator := &lumberjack.Logger{
			Filename:   logFile,
			MaxSize:    1, // megabytes
			MaxBackups: 3,
			MaxAge:     7, //days
			Compress:   false,
		}
		logrus.SetOutput(logFileRotator)
	} else {
		// 否则，输出到标准输出 (stdout)
		logrus.SetOutput(os.Stdout)
	}

	//loglevel
	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		logrus.Warnf("Invalid log level '%s', using 'info'", logLevel)
		level = logrus.InfoLevel
	}
	logrus.SetLevel(level)
	logrus.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})
}

func main() {
	config, err := NewConfig()
	if err != nil {
		logrus.Fatalf("Failed to load config: %v", err)
		os.Exit(1)
	}

	setupLogging(config.LogLevel, config.LogFile)

	if config.ShowVer {
		logrus.Infof("UA-Mask version: %s", version)
		return
	}

	config.LogConfig(version)

	stats := NewStats()
	uaCache := NewUACache(config.CacheSize, config.CacheMaxBytes)
	stats.cache = uaCache
	stats.StartWriter("/tmp/UAmask.stats", 5*time.Second)

	fwManager := configuredFirewallManager(config)
	if fwManager != nil {
		fwManager.Start()
		defer fwManager.Stop()
	}
	handler := NewHTTPHandler(config, stats, uaCache, fwManager)

	server := NewServer(config, handler)

	// Run() 会阻塞，直到发生致命错误
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := server.RunContext(ctx); err != nil {
		logrus.Fatalf("Server failed to run: %v", err)
	}
}
