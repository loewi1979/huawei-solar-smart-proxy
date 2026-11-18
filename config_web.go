package main

import (
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

type ConfigStore struct {
	Path string
	mu   sync.Mutex
}

// Replace uses a same-directory rename; the previous valid file is kept for
// rollback. Runtime configuration stays immutable until an explicit restart.
func atomicConfigWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}
func (s *ConfigStore) Replace(data []byte) error {
	if _, err := ParseConfig(data); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old, err := os.ReadFile(s.Path)
	if err != nil {
		return err
	}
	if _, err = ParseConfig(old); err != nil {
		return fmt.Errorf("current config invalid; refusing to replace rollback copy: %w", err)
	}
	if err = atomicConfigWrite(s.Path+".previous", old); err != nil {
		return err
	}
	return atomicConfigWrite(s.Path, data)
}
func (s *ConfigStore) Rollback() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.Path + ".previous")
	if err != nil {
		return err
	}
	if _, err = ParseConfig(data); err != nil {
		return err
	}
	return atomicConfigWrite(s.Path, data)
}
func (w *WebServer) authorized(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Cache-Control", "no-store")
		rw.Header().Set("X-Content-Type-Options", "nosniff")
		rw.Header().Set("X-Frame-Options", "DENY")
		if w.cfg.Web.AuthEnabled && w.cfg.Web.Token != "" {
			user, pass, ok := r.BasicAuth()
			if !ok || user != "admin" || subtle.ConstantTimeCompare([]byte(pass), []byte(w.cfg.Web.Token)) != 1 {
				rw.Header().Set("WWW-Authenticate", `Basic realm="Huawei Proxy", charset="UTF-8"`)
				http.Error(rw, "Authentication required", 401)
				return
			}
		}
		next.ServeHTTP(rw, r)
	})
}
func (w *WebServer) configRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/config", func(rw http.ResponseWriter, r *http.Request) {
		if w.store == nil || w.cfg.Web.AuthEnabled && w.cfg.Web.Token == "" {
			http.Error(rw, "Config management requires standalone YAML and a token when authentication is enabled", 403)
			return
		}
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(rw, configPage)
	})
	mux.HandleFunc("/api/config", func(rw http.ResponseWriter, r *http.Request) {
		if w.store == nil || w.cfg.Web.AuthEnabled && w.cfg.Web.Token == "" {
			http.Error(rw, "Config management disabled", 403)
			return
		}
		if r.Method == http.MethodGet {
			w.store.mu.Lock()
			data, err := os.ReadFile(w.store.Path)
			w.store.mu.Unlock()
			if err != nil {
				http.Error(rw, "Cannot read configuration", 500)
				return
			}
			rw.Header().Set("Content-Type", "application/yaml")
			rw.Header().Set("Content-Disposition", `attachment; filename="huawei-proxy.yaml"`)
			rw.Write(data)
			return
		}
		if r.Method != http.MethodPost {
			rw.Header().Set("Allow", "GET, POST")
			http.Error(rw, "Method not allowed", 405)
			return
		}
		// Browsers cannot send this custom header through a cross-origin form.
		if r.Header.Get("X-Proxy-Config") != "1" {
			http.Error(rw, "Missing request header", 403)
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, 1<<20))
		if err != nil {
			http.Error(rw, "Config too large", 413)
			return
		}
		if _, err = ParseConfig(data); err != nil {
			http.Error(rw, err.Error(), 400)
			return
		}
		if r.URL.Query().Get("validate") == "1" {
			io.WriteString(rw, "Valid configuration. No changes saved.")
			return
		}
		if err = w.store.Replace(data); err != nil {
			http.Error(rw, err.Error(), 500)
			return
		}
		io.WriteString(rw, "Saved. Restart the service to activate. Previous config is available for rollback.")
	})
	mux.HandleFunc("/api/config/rollback", func(rw http.ResponseWriter, r *http.Request) {
		if w.store == nil || w.cfg.Web.AuthEnabled && w.cfg.Web.Token == "" {
			http.Error(rw, "Config management disabled", 403)
			return
		}
		if r.Method != http.MethodPost || r.Header.Get("X-Proxy-Config") != "1" {
			http.Error(rw, "POST with config header required", 403)
			return
		}
		if err := w.store.Rollback(); err != nil {
			http.Error(rw, err.Error(), 500)
			return
		}
		io.WriteString(rw, "Previous configuration restored. Restart the service to activate.")
	})
}

const configPage = `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Huawei Proxy configuration</title>
<style>body{font:16px system-ui;max-width:1000px;margin:40px auto;padding:20px;background:#101923;color:#eef}textarea{width:100%;height:55vh;background:#182735;color:#eef}button,a,input{margin:12px 12px 12px 0}pre{white-space:pre-wrap}</style>
<h1>Configuration · Backup & restore</h1><p>Changes activate after a service restart. Keep a backup before changing connection details. Backups contain passwords.</p>
<a href="/">Status</a> <a href="/api/config">Download YAML backup</a><br><label>Restore YAML file <input type="file" id="file" accept=".yaml,.yml"></label>
<textarea id="yaml" aria-label="YAML configuration" spellcheck="false"></textarea><br>
<button id="validate">Validate</button><button id="save">Save for next restart</button><button id="rollback">Restore previous configuration</button><pre id="result" role="status"></pre>
<script>
const editor=document.getElementById('yaml'),result=document.getElementById('result');
async function load(){const r=await fetch('/api/config');if(r.ok)editor.value=await r.text();else result.textContent=await r.text()};load();
document.getElementById('file').onchange=async e=>{if(e.target.files[0])editor.value=await e.target.files[0].text()};
async function post(url,body){try{let r=await fetch(url,{method:'POST',headers:{'X-Proxy-Config':'1','Content-Type':'application/yaml'},body});result.textContent=await r.text()}catch(e){result.textContent=String(e)}}
document.getElementById('validate').onclick=()=>post('/api/config?validate=1',editor.value);
document.getElementById('save').onclick=()=>post('/api/config',editor.value);
document.getElementById('rollback').onclick=async()=>{await post('/api/config/rollback','');await load()};
</script></html>`
