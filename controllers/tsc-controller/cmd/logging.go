package main

import (
	"log"
	"os"
	"time"
)

var (
	infoLogger  = log.New(os.Stdout, "INFO: ", log.Ldate|log.Ltime|log.Lshortfile)
	warnLogger  = log.New(os.Stderr, "WARN: ", log.Ldate|log.Ltime|log.Lshortfile)
	errorLogger = log.New(os.Stderr, "ERROR: ", log.Ldate|log.Ltime|log.Lshortfile)
	debugLogger = log.New(os.Stdout, "DEBUG: ", log.Ldate|log.Ltime|log.Lshortfile)
)

func logInfo(format string, args ...interface{}) {
	infoLogger.Printf(format, args...)
}

func logWarn(format string, args ...interface{}) {
	warnLogger.Printf(format, args...)
}

func logError(format string, args ...interface{}) {
	errorLogger.Printf(format, args...)
}

func logDebug(format string, args ...interface{}) {
	debugLogger.Printf(format, args...)
}

func shouldRateLimitLog(lastLog map[string]time.Time, key string, interval time.Duration) bool {
	last, ok := lastLog[key]
	if !ok {
		return true
	}
	return time.Since(last) >= interval
}

func updateLastLog(lastLog map[string]time.Time, key string) {
	lastLog[key] = time.Now()
}