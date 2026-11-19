// The status UI combines cached register data with the reader's polling and
// cadence statistics. HTTP requests leave inverter I/O to the other services.

package main

import (
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// WebServer serves the proxy dashboard, health checks, and standalone YAML UI.
type WebServer struct {
	store   *ConfigStore
	cfg     *Config
	cache   *RegisterCache
	reader  atomic.Pointer[Reader]
	startAt time.Time
}

// NewWebServer records the start time for uptime and supports requests before
// the reader is attached.
func NewWebServer(cfg *Config, cache *RegisterCache) *WebServer {
	return &WebServer{
		cfg:     cfg,
		cache:   cache,
		startAt: time.Now(),
	}
}

// SetReader attaches the reader once it's initialized.
func (w *WebServer) SetReader(r *Reader) {
	w.reader.Store(r)
}

// ListenAndServe starts the web server and shuts down gracefully on context cancel.
func (w *WebServer) ListenAndServe(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", w.handleIndex)
	w.configRoutes(mux)
	mux.HandleFunc("/healthz", func(rw http.ResponseWriter, r *http.Request) { rw.Write([]byte("ok")) })
	// Readiness requires every configured unit's fast groups to be fresh.
	mux.HandleFunc("/readyz", func(rw http.ResponseWriter, r *http.Request) {
		for _, u := range w.cfg.Inverter.UnitIDs {
			for _, g := range w.cfg.RegisterGroups {
				if g.Poll == "fast" {
					if _, _, err := w.cache.Fresh(u, g.Address, g.Count, w.cfg.MaxAge(g.Address, g.Count), time.Now()); err != nil {
						http.Error(rw, "fast measurements unavailable or stale", 503)
						return
					}
				}
			}
		}
		rw.Write([]byte("ready"))
	})

	srv := &http.Server{
		Addr:              w.cfg.Web.Listen,
		Handler:           w.authorized(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	slog.Info("web UI listening", "address", w.cfg.Web.Listen)

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return fmt.Errorf("web server: %w", err)
	}
	return nil
}

// registerGroup is a template-friendly grouping of cached registers.
type registerGroup struct {
	Name      string
	Address   uint16
	Count     uint16
	Poll      string
	Registers []CachedRegister
}

// unitRegisters holds the raw register sections rendered for one unit.
type unitRegisters struct {
	UnitID byte
	Groups []registerGroup
}

// DashboardItem is a single decoded metric for the dashboard.
type DashboardItem struct {
	Label string
	Value string
}

// UnitDashboard holds all decoded metrics for one unit ID.
type UnitDashboard struct {
	UnitID byte
	Items  []DashboardItem
}

// handleIndex builds the raw register tables, the fresh decoded metrics, and
// the cadence table for the dashboard template.
func (w *WebServer) handleIndex(rw http.ResponseWriter, r *http.Request) {
	reader := w.reader.Load()
	var stats ReaderStats
	var fastGroups, slowGroups int
	if reader != nil {
		stats = reader.Stats()
		fastGroups = reader.FastGroupCount()
		slowGroups = reader.SlowGroupCount()
	}
	allRegs := w.cache.All()
	now := time.Now()

	// Use half-open ranges with wider endpoints to represent address 65535.
	type addrRange struct {
		start, end uint32
		index      int
	}
	ranges := make([]addrRange, len(w.cfg.RegisterGroups))
	for i, g := range w.cfg.RegisterGroups {
		ranges[i] = addrRange{start: uint32(g.Address), end: uint32(g.Address) + uint32(g.Count), index: i}
	}

	// Keep ungrouped entries for on-demand reads outside the configured groups.
	unitMap := make(map[byte][]registerGroup)
	unitUngrouped := make(map[byte][]CachedRegister)

	for _, uid := range w.cfg.Inverter.UnitIDs {
		groups := make([]registerGroup, len(w.cfg.RegisterGroups))
		for i, g := range w.cfg.RegisterGroups {
			groups[i] = registerGroup{
				Name:    g.Name,
				Address: g.Address,
				Count:   g.Count,
				Poll:    g.Poll,
			}
		}
		unitMap[uid] = groups
	}

	for _, reg := range allRegs {
		groups, ok := unitMap[reg.UnitID]
		if !ok {
			// The persistent cache can still hold rows for units removed from YAML.
			groups = make([]registerGroup, len(w.cfg.RegisterGroups))
			for i, g := range w.cfg.RegisterGroups {
				groups[i] = registerGroup{
					Name:    g.Name,
					Address: g.Address,
					Count:   g.Count,
					Poll:    g.Poll,
				}
			}
			unitMap[reg.UnitID] = groups
		}

		found := false
		for _, ar := range ranges {
			if uint32(reg.Address) >= ar.start && uint32(reg.Address) < ar.end {
				unitMap[reg.UnitID][ar.index].Registers = append(unitMap[reg.UnitID][ar.index].Registers, reg)
				found = true
				break
			}
		}
		if !found {
			unitUngrouped[reg.UnitID] = append(unitUngrouped[reg.UnitID], reg)
		}
	}

	// Render configured units in YAML order and omit empty register groups.
	var units []unitRegisters
	for _, uid := range w.cfg.Inverter.UnitIDs {
		groups := unitMap[uid]
		var activeGroups []registerGroup
		for _, g := range groups {
			if len(g.Registers) > 0 {
				activeGroups = append(activeGroups, g)
			}
		}
		if ung := unitUngrouped[uid]; len(ung) > 0 {
			activeGroups = append(activeGroups, registerGroup{
				Name:      "ungrouped",
				Poll:      "-",
				Registers: ung,
			})
		}
		if len(activeGroups) > 0 {
			units = append(units, unitRegisters{UnitID: uid, Groups: activeGroups})
		}
	}

	// Render configured metrics only. Raw tables preserve acquisition times
	// for inspection, while decoded cards mark expired or missing data.
	var dashboards []UnitDashboard
	for _, uid := range w.cfg.Inverter.UnitIDs {
		var items []DashboardItem
		for _, f := range dashboardFields {
			if f.Unit1 && uid != w.cfg.Inverter.UnitIDs[0] {
				continue
			}
			if !w.cfg.KnownRange(f.Address, f.Length) {
				continue
			}
			val := DecodeDashboardField(f, w.cache, uid)
			if _, _, err := w.cache.Fresh(uid, f.Address, f.Length, w.cfg.MaxAge(f.Address, f.Length), now); err != nil {
				val = "Unavailable / stale"
			}
			if val == "" {
				continue
			}
			items = append(items, DashboardItem{Label: f.Label, Value: val})
		}
		if len(items) > 0 {
			dashboards = append(dashboards, UnitDashboard{UnitID: uid, Items: items})
		}
	}

	data := struct {
		Cadence    []PollingCadenceStat
		Config     *Config
		Stats      ReaderStats
		Uptime     time.Duration
		Now        time.Time
		CacheSize  int
		FastGroups int
		SlowGroups int
		Dashboards []UnitDashboard
		Units      []unitRegisters
	}{
		Config:     w.cfg,
		Stats:      stats,
		Uptime:     now.Sub(w.startAt).Round(time.Second),
		Now:        now,
		CacheSize:  len(allRegs),
		FastGroups: fastGroups,
		SlowGroups: slowGroups,
		Dashboards: dashboards,
		Units:      units,
	}
	if reader != nil {
		// The reader computes the observed intervals and any eligible Home
		// Assistant suggestion. The template displays the result for each group.
		data.Cadence = reader.CadenceStats(now)
	}

	tmpl, err := template.New("index").Funcs(template.FuncMap{
		"seconds": func(d time.Duration) string { return fmt.Sprintf("%.2f s", d.Seconds()) },
		"hexAddr": func(a uint16) string { return fmt.Sprintf("0x%04X", a) },
		"hexVal":  func(v uint16) string { return fmt.Sprintf("0x%04X", v) },
		"relTime": func(t time.Time) string {
			d := now.Sub(t)
			switch {
			case d < time.Second:
				return "just now"
			case d < time.Minute:
				return fmt.Sprintf("%ds ago", int(d.Seconds()))
			case d < time.Hour:
				return fmt.Sprintf("%dm %ds ago", int(d.Minutes()), int(d.Seconds())%60)
			default:
				return fmt.Sprintf("%dh %dm ago", int(d.Hours()), int(d.Minutes())%60)
			}
		},
		"regName": func(a uint16) string { return RegisterName(a) },
		"isContReg": func(a uint16) bool {
			_, ok := registerDefs[a]
			return !ok && RegisterName(a) != ""
		},
	}).Parse(indexTemplate)
	if err != nil {
		http.Error(rw, "template error: "+err.Error(), 500)
		return
	}

	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(rw, data); err != nil {
		slog.Warn("web: template execute", "error", err)
	}
}

// indexTemplate renders status, measured cadence, decoded metrics, and raw words.
const indexTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="5">
<title>Huawei Solar Smart Proxy</title>
<style>
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #f5f5f5; color: #333; padding: 1rem; }
  h1 { margin-bottom: 1rem; font-size: 1.4rem; }
  .cards { display: flex; gap: 1rem; flex-wrap: wrap; margin-bottom: 1.5rem; }
  .card { background: #fff; border-radius: 8px; padding: 1rem 1.25rem; box-shadow: 0 1px 3px rgba(0,0,0,.1); min-width: 220px; flex: 1; }
  .card h2 { font-size: .85rem; text-transform: uppercase; color: #888; margin-bottom: .5rem; letter-spacing: .05em; }
  .card dl { display: grid; grid-template-columns: auto 1fr; gap: .25rem .75rem; font-size: .9rem; }
  .card dt { color: #666; }
  .card dd { font-weight: 500; }
  .group { margin-bottom: 1.5rem; }
  .group-header { display: flex; align-items: baseline; gap: .75rem; margin-bottom: .5rem; }
  .group-header h3 { font-size: 1rem; }
  .group-header .badge { font-size: .75rem; padding: .1rem .5rem; border-radius: 4px; color: #fff; }
  .badge-fast { background: #2563eb; }
  .badge-slow { background: #7c3aed; }
  .badge-other { background: #6b7280; }
  table { width: 100%; border-collapse: collapse; background: #fff; border-radius: 8px; overflow: hidden; box-shadow: 0 1px 3px rgba(0,0,0,.1); font-size: .85rem; }
  th { background: #f9fafb; text-align: left; padding: .5rem .75rem; font-weight: 600; color: #555; border-bottom: 2px solid #e5e7eb; }
  td { padding: .4rem .75rem; border-bottom: 1px solid #f0f0f0; font-variant-numeric: tabular-nums; }
  tr:hover td { background: #f9fafb; }
  .mono { font-family: "SF Mono", "Cascadia Code", "Fira Code", monospace; }
  .muted { color: #999; font-size: .8rem; }
  footer { margin-top: 2rem; text-align: center; color: #aaa; font-size: .8rem; }
</style>
</head>
<body>
<h1>Huawei Solar Smart Proxy</h1>
<p><a href="/config">Configuration · Backup &amp; restore</a></p>

<div class="cards">
  <div class="card">
    <h2>Connection</h2>
    <dl>
      <dt>Inverter</dt><dd>{{.Config.InverterAddr}}</dd>
      <dt>Unit IDs</dt><dd>{{range $i, $uid := .Config.Inverter.UnitIDs}}{{if $i}}, {{end}}{{$uid}}{{end}}</dd>
      <dt>Modbus Server</dt><dd>{{.Config.Server.Listen}}</dd>
      <dt>Forward Unknown</dt><dd>{{.Config.ForwardUnknownReads}}</dd>
      <dt>Read Only</dt><dd>{{.Config.ReadOnly}}</dd>
    </dl>
  </div>
  <div class="card">
    <h2>Polling</h2>
    <dl>
      <dt>Read Pause</dt><dd>{{.Config.Polling.ReadPauseMs}}ms</dd>
      <dt>Fast Interval</dt><dd>{{.Config.Polling.FastIntervalS}}s</dd>
      <dt>Fast / Slow Max Age</dt><dd>{{.Config.Polling.FastMaxAgeS}}s / {{.Config.Polling.SlowMaxAgeS}}s</dd>
      <dt>Slow Interval</dt><dd>{{.Config.Polling.SlowIntervalS}}s</dd>
      <dt>Fast Groups</dt><dd>{{.FastGroups}}</dd>
      <dt>Slow Groups</dt><dd>{{.SlowGroups}}</dd>
    </dl>
  </div>
  <div class="card">
    <h2>Runtime</h2>
    <dl>
      <dt>Uptime</dt><dd>{{.Uptime}}</dd>
      <dt>Poll Attempts</dt><dd>{{.Stats.CycleCount}}</dd>
      <dt>Last Poll Attempt</dt><dd>{{if .Stats.LastCycleTime.IsZero}}-{{else}}{{relTime .Stats.LastCycleTime}}{{end}}</dd>
      <dt>Request Duration</dt><dd>{{if eq .Stats.CycleCount 0}}-{{else}}{{.Stats.LastCycleDur}}{{end}}</dd>
      <dt>Cached Registers</dt><dd>{{.CacheSize}}</dd>
    </dl>
  </div>
</div>

{{if .Cadence}}
<div class="group">
  <h2 style="font-size: 1.15rem; margin-bottom: .5rem;">Measured polling intervals</h2>
  <p class="muted" style="margin-bottom: .75rem;">Last 100 successful acquisition intervals per group, since restart. Unchanged measurements count; failure and reconnect delays are included in the next successful interval.</p>
  <div style="overflow-x: auto;">
  <table>
    <thead><tr><th>Unit / Group</th><th>Tier</th><th>Intervals</th><th>Median</th><th>Last / Longest</th><th>Last success</th><th>Suggested HA interval</th></tr></thead>
    <tbody>
    {{range .Cadence}}
      <tr>
        <td>{{.UnitID}} / {{.Group}}</td><td>{{.Poll}}</td><td>{{.Samples}} / 100</td>
        <td>{{if .Samples}}{{seconds .Median}}{{else}}Collecting…{{end}}</td>
        <td>{{if .Samples}}{{seconds .LastGap}} / {{seconds .Maximum}}{{else}}—{{end}}</td>
        <td>{{if .LastSuccess.IsZero}}Waiting for first success{{else}}{{relTime .LastSuccess}}{{if .Stale}} · stale{{end}}{{end}}</td>
        <td>{{if eq .Poll "slow"}}—{{else if .Stale}}Unavailable{{else if .RecommendedHASecs}}{{.RecommendedHASecs}} s{{else}}Collecting (minimum 5 intervals){{end}}</td>
      </tr>
    {{end}}
    </tbody>
  </table>
  </div>
  <p class="muted" style="margin-top: .75rem;">HA suggestion: floor(median ÷ 2), minimum 1 second. Available for fresh fast groups after 5 intervals; the window fills up to 100. Refresh inverter and meter devices separately. This only reads the proxy cache, does not speed up the dongle and does not change HA settings automatically.</p>
</div>
{{end}}

{{if .Dashboards}}
<div class="cards">
{{range .Dashboards}}
  <div class="card">
    <h2>Unit {{.UnitID}}</h2>
    <dl>
    {{range .Items}}
      <dt>{{.Label}}</dt><dd>{{.Value}}</dd>
    {{end}}
    </dl>
  </div>
{{end}}
</div>
{{end}}

{{range .Units}}
<h2 style="margin: 1.5rem 0 .75rem; font-size: 1.15rem;">Unit ID {{.UnitID}}</h2>
{{range .Groups}}
<div class="group">
  <div class="group-header">
    <h3>{{.Name}}</h3>
    {{if eq .Poll "fast"}}<span class="badge badge-fast">fast</span>
    {{else if eq .Poll "slow"}}<span class="badge badge-slow">slow</span>
    {{else}}<span class="badge badge-other">{{.Poll}}</span>{{end}}
    {{if .Address}}<span class="muted">{{.Address}} &ndash; {{printf "%d" (len .Registers)}} registers</span>{{end}}
  </div>
  <table>
    <thead><tr><th>Name</th><th>Address</th><th>Hex</th><th>Value</th><th>Hex Value</th><th>Updated</th></tr></thead>
    <tbody>
    {{range .Registers}}
    <tr>
      <td{{if isContReg .Address}} class="muted"{{end}}>{{regName .Address}}</td>
      <td class="mono">{{.Address}}</td>
      <td class="mono muted">{{hexAddr .Address}}</td>
      <td class="mono">{{.Value}}</td>
      <td class="mono muted">{{hexVal .Value}}</td>
      <td class="muted">{{relTime .UpdatedAt}}</td>
    </tr>
    {{end}}
    </tbody>
  </table>
</div>
{{end}}
{{end}}

<footer>Auto-refreshes every 5s &middot; {{.Now.Format "15:04:05"}}</footer>
</body>
</html>
`
