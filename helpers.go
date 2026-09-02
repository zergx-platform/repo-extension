package main

import (
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// errNotFoundForHTTP mirrors errNotFoundForHTTP for the jj client.
var errNotFoundForHTTP = errors.New("not found")

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envInt(k string, d int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return d
}

func envNormalizePort(v string) string {
	if i := strings.LastIndexByte(v, ':'); i >= 0 {
		if c := v[i+1:]; c != "" {
			return c
		}
	}
	return v
}

var sessionComponentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)

func validSessionComponent(s string) bool {
	if !sessionComponentRe.MatchString(s) {
		return false
	}
	if strings.Contains(s, ":") || strings.Contains(s, "..") ||
		strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") ||
		strings.Contains(s, "//") || strings.HasSuffix(s, ".") || strings.HasSuffix(s, ".lock") {
		return false
	}
	return true
}

func namingSession(org, repo, bookmark string) string {
	return org + ":" + repo + ":" + bookmark
}

func parseSession(name string) (org, repo, bookmark string, ok bool) {
	parts := strings.Split(name, ":")
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}
