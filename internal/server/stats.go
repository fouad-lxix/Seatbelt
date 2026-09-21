package server

import (
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/fouad-lxix/seatbelt/internal/stats"
)

// defaultRefreshSeconds is how often /stats/ui reloads itself.
const defaultRefreshSeconds = 3

// report assembles the live picture from the pools, the limiters and the
// budget tracker. Numbers are read independently, so a report under heavy
// traffic can be very slightly inconsistent with itself. That is an
// acceptable trade against locking the whole request path.
func (s *Server) report() stats.Report {
	names := make([]string, 0, len(s.deps.Pools))
	for name := range s.deps.Pools {
		names = append(names, name)
	}
	sort.Strings(names)

	providers := make([]stats.Provider, 0, len(names))
	for _, name := range names {
		p := s.deps.Pools[name]

		var rl stats.RateLimit
		if b := s.deps.Limiters[name]; b != nil {
			rl = stats.RateLimit{
				PerMinute: b.PerMinute(),
				Burst:     b.Capacity(),
				Headroom:  b.Tokens(),
			}
		}

		providers = append(providers, stats.Provider{
			Name:          name,
			Workers:       p.Workers(),
			InFlight:      p.InFlight(),
			QueueDepth:    p.QueueDepth(),
			QueueCapacity: p.QueueCapacity(),
			RateLimit:     rl,
			Counts:        p.Counts(),
		})
	}

	b := s.deps.Budget.Snapshot()

	asOf, source := "unknown", "unknown"
	if s.deps.Pricing != nil {
		t := s.deps.Pricing.Table()
		asOf, source = t.AsOf(), t.Source()
	}

	return stats.Report{
		Uptime:        time.Since(s.started).Round(time.Second).String(),
		PricingAsOf:   asOf,
		PricingSource: source,
		Budget: stats.Budget{
			CapUSD:        b.CapUSD,
			SpentUSD:      b.SpentUSD,
			RemainingUSD:  b.RemainingUSD,
			Calls:         b.Calls,
			UnpricedCalls: b.UnpricedCalls,
			Halted:        b.Halted,
			HaltReason:    b.HaltReason,
		},
		Providers: providers,
		Combined:  stats.Combine(providers),
	}
}

func (s *Server) statsJSON(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.report())
}

// statsUI serves the same numbers as a plain HTML page, refreshed with a
// meta tag rather than JavaScript so it works with scripting disabled.
func (s *Server) statsUI(w http.ResponseWriter, r *http.Request) {
	refresh := defaultRefreshSeconds
	if raw := r.URL.Query().Get("refresh"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 && n <= 3600 {
			refresh = n // 0 disables auto-refresh
		}
	}

	page := struct {
		stats.Report
		RefreshSeconds int
		Now            string
	}{
		Report:         s.report(),
		RefreshSeconds: refresh,
		Now:            time.Now().Format("15:04:05"),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := statsTemplate.Execute(w, page); err != nil {
		writeLogf("could not render the stats page: %v", err)
	}
}

// statsTemplate is parsed once at startup. html/template escapes values
// by context, so provider names and halt reasons cannot inject markup.
var statsTemplate = template.Must(template.New("stats").Funcs(template.FuncMap{
	"usd": func(v float64) string { return "$" + strconv.FormatFloat(v, 'f', 4, 64) },
	"pct": func(part, whole float64) float64 {
		if whole <= 0 {
			return 0
		}
		p := part / whole * 100
		if p > 100 {
			return 100
		}
		return p
	},
	"num": func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) },
}).Parse(statsHTML))

const statsHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Seatbelt</title>
{{if .RefreshSeconds}}<meta http-equiv="refresh" content="{{.RefreshSeconds}}">{{end}}
<style>
  :root { color-scheme: light dark; }
  body {
    font: 14px/1.5 ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
    margin: 0; padding: 2rem; max-width: 60rem;
    background: Canvas; color: CanvasText;
  }
  h1 { font-size: 1.25rem; margin: 0 0 .25rem; }
  .sub { opacity: .65; margin: 0 0 1.5rem; font-size: .85rem; }
  h2 { font-size: 1rem; margin: 2rem 0 .5rem; }
  table { border-collapse: collapse; width: 100%; }
  th, td { text-align: right; padding: .4rem .6rem; border-bottom: 1px solid color-mix(in srgb, CanvasText 15%, transparent); }
  th:first-child, td:first-child { text-align: left; }
  th { font-weight: 600; opacity: .7; font-size: .8rem; text-transform: uppercase; letter-spacing: .03em; }
  tr.total td { font-weight: 600; border-top: 2px solid color-mix(in srgb, CanvasText 30%, transparent); border-bottom: none; }
  .bar { height: .5rem; border-radius: 99px; background: color-mix(in srgb, CanvasText 12%, transparent); overflow: hidden; margin-top: .4rem; }
  .bar > i { display: block; height: 100%; background: currentColor; }
  .money { font-size: 1.5rem; font-weight: 600; margin: 0; }
  .halted { padding: .75rem 1rem; border-radius: .4rem; margin: 1rem 0;
            background: color-mix(in srgb, crimson 15%, Canvas); border: 1px solid crimson; }
  code { font-family: ui-monospace, Consolas, monospace; font-size: .85em; }
  footer { margin-top: 2.5rem; font-size: .8rem; opacity: .6; }
</style>
</head>
<body>

<h1>Seatbelt</h1>
<p class="sub">up {{.Uptime}} &middot; refreshed {{.Now}}{{if .RefreshSeconds}} &middot; every {{.RefreshSeconds}}s{{else}} &middot; auto-refresh off{{end}}</p>

{{if .Budget.Halted}}
<div class="halted">
  <strong>Stopped.</strong> {{.Budget.HaltReason}}<br>
  No further outbound calls will be made.
</div>
{{end}}

<h2>Budget</h2>
<p class="money">{{usd .Budget.SpentUSD}} <span style="opacity:.55;font-size:1rem">of {{usd .Budget.CapUSD}}</span></p>
<div class="bar"><i style="width:{{pct .Budget.SpentUSD .Budget.CapUSD}}%"></i></div>
<table>
  <tr><td>Remaining</td><td>{{usd .Budget.RemainingUSD}}</td></tr>
  <tr><td>Calls priced</td><td>{{.Budget.Calls}}</td></tr>
  <tr><td>Calls that could not be priced</td><td>{{.Budget.UnpricedCalls}}</td></tr>
</table>

<h2>Providers</h2>
<table>
  <thead>
    <tr>
      <th>Provider</th><th>In flight</th><th>Queue</th><th>Headroom</th>
      <th>Requests</th><th>Succeeded</th><th>Failed</th><th>Retried</th><th>Rejected</th><th>Spend</th>
    </tr>
  </thead>
  <tbody>
  {{range .Providers}}
    <tr>
      <td>{{.Name}}</td>
      <td>{{.InFlight}} / {{.Workers}}</td>
      <td>{{.QueueDepth}} / {{.QueueCapacity}}</td>
      <td>{{printf "%.1f" .RateLimit.Headroom}} / {{num .RateLimit.Burst}}<br>
          <span style="opacity:.55;font-size:.8em">{{num .RateLimit.PerMinute}}/min</span></td>
      <td>{{.Counts.Requests}}</td>
      <td>{{.Counts.Succeeded}}</td>
      <td>{{.Counts.Failed}}</td>
      <td>{{.Counts.Retried}}</td>
      <td>{{.Counts.Rejected}}</td>
      <td>{{usd .Counts.SpendUSD}}</td>
    </tr>
  {{else}}
    <tr><td colspan="10">No providers are configured.</td></tr>
  {{end}}
  </tbody>
  <tfoot>
    <tr class="total">
      <td>Combined</td>
      <td>{{.Combined.InFlight}}</td>
      <td>{{.Combined.QueueDepth}} / {{.Combined.QueueCapacity}}</td>
      <td></td>
      <td>{{.Combined.Counts.Requests}}</td>
      <td>{{.Combined.Counts.Succeeded}}</td>
      <td>{{.Combined.Counts.Failed}}</td>
      <td>{{.Combined.Counts.Retried}}</td>
      <td>{{.Combined.Counts.Rejected}}</td>
      <td>{{usd .Combined.Counts.SpendUSD}}</td>
    </tr>
  </tfoot>
</table>

<footer>
  Prices as of {{.PricingAsOf}} ({{.PricingSource}}), refreshed daily from the
  published table. Spot-check against your billing page before relying on them.<br>
  Machine-readable at <code>/stats</code>.
  Add <code>?refresh=10</code> to slow this page down, or <code>?refresh=0</code> to stop it.
</footer>

</body>
</html>
`
