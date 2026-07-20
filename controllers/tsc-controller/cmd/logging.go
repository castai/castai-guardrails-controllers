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

	// oncePerInterval prevents duplicate logs within the same interval
	// by tracking the last log time per unique key
)

// logAtLevel logs a message at the specified level with rate limiting
func logAtLevel(level int, key string, format string, args ...interface{}) {
	if level < LogLevel {
		return
	}

	logTrackersLock.Lock()
	defer logTrackersLock.Unlock()

	now := time.Now()
	last, ok := logTrackers[key]
	interval := currentLogInterval

	// Always log if we haven't seen this key before
	// or if enough time has passed since last log
	if !ok || now.Sub(last) > interval {
		log.Printf(format, args...)
		logTrackers[key] = now
	}
}

// logDebug logs a debug message with rate limiting
// Key is used to identify the log type for rate limiting
func logDebug(key string, format string, args ...interface{}) {
	logAtLevel(sevDebug, key, "[DEBUG] "+format, args...)
}

// logInfo logs an info message with rate limiting
// Key is used to identify the log type for rate limiting
func logInfo(key string, format string, args ...interface{}) {
	logAtLevel(sevInfo, key, "[INFO] "+format, args...)
}

// logWarn logs a warning message with rate limiting
// Key is used to identify the log type for rate limiting
func logWarn(key string, format string, args ...interface{}) {
	logAtLevel(sevWarn, key, "[WARN] "+format, args...)
}

// logError logs an error message with rate limiting
// Key is used to identify the log type for rate limiting
func logError(key string, format string, args ...interface{}) {
	logAtLevel(sevError, key, "[ERROR] "+format, args...)
}

// logAlways logs a message without rate limiting
// Use sparingly for critical startup/shutdown messages
func logAlways(format string, args ...interface{}) {
	log.Printf(format, args...)
}

// SetLogInterval updates the log interval for rate limiting
// This is called when the ConfigMap is updated
func SetLogInterval(interval time.Duration) {
	logTrackersLock.Lock()
	defer logTrackersLock.Unlock()
	currentLogInterval = interval
	logAlways("Log interval updated to %v", interval)
}

// GetLogInterval returns the current log interval
func GetLogInterval() time.Duration {
	logTrackersLock.Lock()
	defer logTrackersLock.Unlock()
	return currentLogInterval
}

// ResetLogTracker resets the rate limiting tracker for a specific key
// Useful for testing or when you need to force a log
func ResetLogTracker(key string) {
	logTrackersLock.Lock()
	defer logTrackersLock.Unlock()
	delete(logTrackers, key)
}

// initLogging initializes the logging system
func initLogging() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	logAlways("TSC Controller starting...")
}

