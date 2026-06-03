package console

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	consoleSessionCookie = "codex_gitea_console"
	consoleSessionMaxAge = 12 * time.Hour
)

// consoleAuthMiddleware guards the console with an in-app login page instead of
// HTTP Basic Auth. When password is empty the console is disabled, so a
// misconfigured deployment never exposes an open panel.
func consoleAuthMiddleware(passwordFn func() string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		password := ""
		if passwordFn != nil {
			password = passwordFn()
		}
		if password == "" {
			writeDisabledConsole(w)
			return
		}

		switch {
		case r.URL.Path == "/admin/login" && r.Method == http.MethodGet:
			writeLoginPage(w, "")
			return
		case r.URL.Path == "/admin/login" && r.Method == http.MethodPost:
			handleConsoleLogin(w, r, password)
			return
		case r.URL.Path == "/admin/logout":
			clearConsoleSession(w, r)
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}

		if !validConsoleSession(r, password) {
			if strings.HasPrefix(r.URL.Path, "/admin/api/") {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
				return
			}
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func handleConsoleLogin(w http.ResponseWriter, r *http.Request, password string) {
	if err := r.ParseForm(); err != nil {
		writeLoginPage(w, "无法读取登录表单")
		return
	}
	pass := r.FormValue("password")
	if subtle.ConstantTimeCompare([]byte(pass), []byte(password)) != 1 {
		writeLoginPage(w, "密码不正确")
		return
	}
	setConsoleSession(w, r, password)
	http.Redirect(w, r, "/admin/", http.StatusSeeOther)
}

func setConsoleSession(w http.ResponseWriter, r *http.Request, password string) {
	issued := time.Now().Unix()
	raw := strconv.FormatInt(issued, 10)
	value := raw + "." + signConsoleSession(raw, password)
	http.SetCookie(w, &http.Cookie{
		Name:     consoleSessionCookie,
		Value:    value,
		Path:     "/admin/",
		MaxAge:   int(consoleSessionMaxAge.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
	})
}

func clearConsoleSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     consoleSessionCookie,
		Value:    "",
		Path:     "/admin/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
	})
}

func validConsoleSession(r *http.Request, password string) bool {
	cookie, err := r.Cookie(consoleSessionCookie)
	if err != nil {
		return false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 2 {
		return false
	}
	issued, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return false
	}
	if time.Since(time.Unix(issued, 0)) > consoleSessionMaxAge || time.Now().Unix() < issued {
		return false
	}
	want := signConsoleSession(parts[0], password)
	return subtle.ConstantTimeCompare([]byte(parts[1]), []byte(want)) == 1
}

func signConsoleSession(value, password string) string {
	mac := hmac.New(sha256.New, []byte(password))
	mac.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func writeLoginPage(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if message != "" {
		w.WriteHeader(http.StatusUnauthorized)
	}
	_, _ = fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>登录 codex-gitea</title>
<style>
  :root {
    --bg: #f7f7f5; --panel: #fff; --text: #171717; --muted: #737373;
    --line: #e5e5e3; --line-strong: #d4d4d0; --black: #101010; --red: #b42318;
    --shadow: 0 1px 2px rgba(16,16,16,.04), 0 18px 54px rgba(16,16,16,.08);
  }
  * { box-sizing: border-box; }
  body {
    min-height: 100vh; margin: 0; color: var(--text);
    display: grid; place-items: center; padding: 24px;
    background:
      linear-gradient(rgba(229,229,227,.54) 1px, transparent 1px),
      linear-gradient(90deg, rgba(229,229,227,.54) 1px, transparent 1px),
      var(--bg);
    background-size: 42px 42px;
    font-family: ui-sans-serif, -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif;
  }
  .shell { width: min(100%%, 420px); }
  .brand { display: flex; align-items: center; gap: 12px; margin-bottom: 18px; }
  .mark {
    width: 34px; height: 34px; border-radius: 8px; display: grid; place-items: center;
    background: var(--black); color: #fff; font-weight: 760; font-size: 15px;
  }
  h1 { margin: 0; font-size: 16px; line-height: 1.2; }
  .subtitle { margin: 2px 0 0; color: var(--muted); font-size: 12px; }
  .card {
    padding: 22px; border: 1px solid var(--line); border-radius: 10px;
    background: var(--panel); box-shadow: var(--shadow);
  }
  .eyebrow { margin: 0 0 8px; color: var(--muted); font-size: 12px; font-weight: 650; }
  h2 { margin: 0 0 8px; font-size: 28px; line-height: 1.08; letter-spacing: 0; }
  .hint { margin: 0 0 18px; color: var(--muted); font-size: 13px; }
  label { display: block; margin: 0 0 6px; color: var(--muted); font-size: 12px; font-weight: 650; }
  input {
    width: 100%%; min-height: 40px; padding: 8px 10px; border: 1px solid var(--line-strong);
    border-radius: 8px; background: #fff; color: var(--text); font-size: 14px; outline: none;
  }
  input:focus { border-color: var(--black); box-shadow: 0 0 0 3px rgba(16,16,16,.08); }
  button {
    width: 100%%; min-height: 40px; margin-top: 14px; border: 1px solid var(--black);
    border-radius: 8px; background: var(--black); color: #fff; font-size: 13px; font-weight: 700;
    cursor: pointer;
  }
  .error { margin: 12px 0 0; color: var(--red); font-size: 12px; min-height: 18px; }
  .foot { margin-top: 14px; color: var(--muted); font-size: 12px; text-align: center; }
</style>
</head>
<body>
<main class="shell">
  <div class="brand">
    <div class="mark">cg</div>
    <div>
      <h1>codex-gitea</h1>
      <p class="subtitle">Review automation console</p>
    </div>
  </div>
  <section class="card">
    <p class="eyebrow">Admin access</p>
    <h2>登录控制台</h2>
    <p class="hint">输入管理员密码以管理 webhook、Codex 鉴权和 review 任务。</p>
    <form method="post" action="/admin/login">
      <label for="password">管理员密码</label>
      <input id="password" name="password" type="password" autocomplete="current-password" autofocus>
      <button type="submit">登录</button>
      <div class="error">%s</div>
    </form>
  </section>
  <p class="foot">Session expires after 12 hours.</p>
</main>
</body>
</html>`, html.EscapeString(message))
}

func writeDisabledConsole(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`<!DOCTYPE html><html lang="zh-CN"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>控制台未启用</title><body style="margin:0;min-height:100vh;display:grid;place-items:center;background:#f7f7f5;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI','PingFang SC','Microsoft YaHei',sans-serif;color:#171717"><main style="width:min(100%,420px);padding:22px;border:1px solid #e5e5e3;border-radius:10px;background:#fff;box-shadow:0 18px 54px rgba(16,16,16,.08)"><h1 style="margin:0 0 8px;font-size:24px">控制台未启用</h1><p style="margin:0;color:#737373;font-size:14px">请设置 ADMIN_PASSWORD 后重启服务。</p></main></body></html>`))
}
