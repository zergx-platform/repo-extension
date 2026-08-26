package main

import "log/slog"

// log is the application-level structured logger (slog JSON by default,
// overridable via the standard slog handler setup). The SDK stays silent and
// surfaces errors through return values; logging happens only here.
var log = slog.Default().With("svc", "repo-extension")
