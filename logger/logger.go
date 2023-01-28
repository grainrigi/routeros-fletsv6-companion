// from https://zenn.dev/tharu/articles/8c2ec139615fc4
package logger

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
)

const totalStep = 5

const (
	ERROR = iota + 1
	WARNING
	INFO
	DEBUG
	TRACE
)

func SetLogLevel() int {
	logLevel := os.Getenv("LOG_LEVEL")
	switch logLevel {
	case "INFO", "info":
		return INFO
	case "DEBUG", "debug":
		return DEBUG
	case "TRACE", "trace":
		return TRACE
	case "ERROR", "error":
		return ERROR
	case "WARNING", "warning":
		return WARNING
	default:
		return INFO
	}
}

type BuiltinLogger struct {
	logger *log.Logger
	level  int
}

func NewBuiltinLogger() *BuiltinLogger {
	return &BuiltinLogger{
		logger: log.Default(),
		level:  SetLogLevel(),
	}
}

func (l *BuiltinLogger) Trace(format string, args ...any) {
	if l.level >= TRACE {
		l.logger.SetOutput(os.Stdout)
		l.logger.SetFlags(log.Ldate | log.Ltime)

		_, file, line, ok := runtime.Caller(1)
		if ok {
			caller := fmt.Sprintf("%s:%d: ", filepath.Base(file), line)
			l.logger.Printf("[TRACE] "+caller+format, args...)
		} else {
			l.logger.Printf("[TRACE] "+format, args...)
		}
	}
}

func (l *BuiltinLogger) Debug(format string, args ...any) {
	if l.level >= DEBUG {
		l.logger.SetOutput(os.Stdout)
		l.logger.SetFlags(log.Ldate | log.Ltime)

		_, file, line, ok := runtime.Caller(1)
		if ok {
			caller := fmt.Sprintf("%s:%d: ", filepath.Base(file), line)
			l.logger.Printf("[DEBUG] "+caller+format, args...)
		} else {
			l.logger.Printf("[DEBUG] "+format, args...)
		}
	}
}

func (l *BuiltinLogger) Info(format string, args ...any) {
	if l.level >= INFO {
		l.logger.SetOutput(os.Stdout)
		format = "[INFO]  " + format
		l.logger.SetFlags(log.Ldate | log.Ltime)
		l.logger.Printf(format, args...)
	}
}

func (l *BuiltinLogger) Warning(format string, args ...any) {
	if l.level >= WARNING {
		l.logger.SetOutput(os.Stdout)
		format = "[WARN]  " + format
		l.logger.SetFlags(log.Ldate | log.Ltime)
		l.logger.Printf(format, args...)
	}
}

func (l *BuiltinLogger) Error(format string, args ...any) {
	if l.level >= ERROR {
		l.logger.SetOutput(os.Stdout)
		l.logger.SetFlags(log.Ldate | log.Ltime)

		_, file, line, ok := runtime.Caller(1)
		if ok {
			caller := fmt.Sprintf("%s:%d: ", filepath.Base(file), line)
			l.logger.Printf("[ERROR] "+caller+format, args...)
		} else {
			l.logger.Printf("[ERROR] "+format, args...)
		}
	}
}

func (l *BuiltinLogger) Fatal(format string, args ...any) {
	if l.level >= ERROR {
		l.logger.SetOutput(os.Stdout)
		l.logger.SetFlags(log.Ldate | log.Ltime)

		_, file, line, ok := runtime.Caller(1)
		if ok {
			caller := fmt.Sprintf("%s:%d: ", filepath.Base(file), line)
			l.logger.Fatalf("[FATAL] "+caller+format, args...)
		} else {
			l.logger.Fatalf("[FATAL] "+format, args...)
		}
	}
}
