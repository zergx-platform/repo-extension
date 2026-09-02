package main

import (
	"context"

	"github.com/abcp-sdk/abc-protocol-go/extension"
)

// localeOf resolves the session's effective locale for a tool call. It reads
// the agent-projected `vars.agent.locale` (provider "agent") and falls back to
// the env default.
func localeOf(ctx context.Context, ext *extension.Extension, sessionName, fallback string) string {
	if ext == nil || sessionName == "" {
		return fallback
	}
	v := ext.GetSessionVariable(ctx, "agent", sessionName, "locale", "")
	if v == "" {
		return fallback
	}
	return v
}

// isZh reports whether a locale is Chinese-family (zh / zh-cn / zh-hans/…).
func isZh(locale string) bool {
	return len(locale) >= 2 && locale[0] == 'z' && locale[1] == 'h'
}

// t picks the localized prose: English default, or the zh override.
func t(locale, en, zh string) string {
	if isZh(locale) {
		return zh
	}
	return en
}

// lc returns the localized Content for a tool result given the session locale,
// the English string `en` and its Chinese equivalent `zh`.
func lc(ctx context.Context, ext *extension.Extension, sessionName, en, zh string) string {
	return t(localeOf(ctx, ext, sessionName, envOr("ZERGX_LOCALE", "en")), en, zh)
}
