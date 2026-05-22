package main

import (
	"log"
	"os"
	"sync"
	"time"
)

const (
	sevDebug = iota
	sevInfo
	sevWarn
	sevError
)

var (
	// LogLevel controls the minimum severity to log
	LogLevel = sevInfo

	// logTrackers tracks the last time a log message was emitted for rate limiting
	logTrackers     = make(map[string]time.Time)
	logTrackersLock sync.Mutex

	// Default log interval for rate limiting
	DefaultLogInterval = 15 * time.Minute

	// currentLogInterval can be updated via ConfigMap
	currentLogInterval = DefaultLogInterval
)

// logAtLevel logs a message at the specified level with rate limiting
func logAtLevel(level int, key string, format string, args ...interface{}) {
	if level < LogLevel {
		return
	}

	logTrackersLock.Lock()
	defer logTrackersLock.Unlock()

	last, ok := logTrackers[key]
	interval := currentLogInterval

	if !ok || time.Since(last) > interval {
		log.Printf(format, args...)
		logTrackers[key] = time.Now()
	}
}

// Debug logs a debug message with rate limiting
func logDebug(key string, format string, args ...interface{}) {
	logAtLevel(sevDebug, key, "[DEBUG] "+format, args...)
}

// Info logs an info message with rate limiting
func logInfo(key string, format string, args ...interface{}) {
	logAtLevel(sevInfo, key, "[INFO] "+format, args...)
}

// Warn logs a warning message with rate limiting
func logWarn(key string, format string, args ...interface{}) {
	logAtLevel(sevWarn, key, "[WARN] "+format, args...)
}

// Error logs an error message with rate limiting
func logError(key string, format string, args ...interface{}) {
	logAtLevel(sevError, key, "[ERROR] "+format, args...)
}

// LogAlways logs a message without rate limiting (use sparingly)
func logAlways(format string, args ...interface{}) {
	log.Printf(format, args...)
}

// SetLogInterval updates the log interval for rate limiting
func SetLogInterval(interval time.Duration) {
	logTrackersLock.Lock()
	defer logTrackersLock.Unlock()
	currentLogInterval = interval
}

// GetLogInterval returns the current log interval
func GetLogInterval() time.Duration {
	logTrackersLock.Lock()
	defer logTrackersLock.Unlock()
	return currentLogInterval
}

// initLogging initializes the logging system
func initLogging() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	logAlways("JVM Probe Controller starting...")
}