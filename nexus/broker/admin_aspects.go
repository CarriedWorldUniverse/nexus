package broker

// Admin UI aspects settings view.
//
// Renders a per-aspect card with editable primary/judge/compact model fields
// and a dispatch toggle. Inline editing via HTMX POST.

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/CarriedWorldUniverse/nexus/nexus/credentials"
	"github.com/CarriedWorldUniverse/nexus/shared/schemas"
)

// handleAdminAspectsList returns the aspects settings page.
func (b *Broker) handleAdminAspectsList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")

	agents, err := b.getAgentList(r)
	if err != nil {
		http.Error(w, "failed to fetch agents", http.StatusInternalServerError)
		return
	}

	creds, err := b.getCredentials(r)
	if err != nil {
		http.Error(w, "failed to fetch credentials", http.StatusInternalServerError)
		return
	}

	var buf strings.Builder

	fmt.Fprintf(&buf, `<div class="settings-view">`)
	fmt.Fprintf(&buf, `  <h2>Aspect Configuration</h2>`)
	fmt.Fprintf(&buf, `  <p>Per-aspect model + credential overrides.</p>`)
	fmt.Fprintf(&buf, `  <div class="mt-16">`)

	for _, agent := range agents {
		buf.WriteString(b.renderAspectCard(agent, creds))
	}

	fmt.Fprintf(&buf, `  </div>`)
	fmt.Fprintf(&buf, `</div>`)

	w.Write([]byte(buf.String()))
}

func (b *Broker) renderAspectCard(agent adminRosterAspect, creds []credentials.Metadata) string {
	var buf strings.Builder

	// Build credential options
	var credOpts strings.Builder
	credOpts.WriteString(`<option value="">(use keyfile default)</option>`)
	for _, c := range creds {
		fmt.Fprintf(&credOpts, `<option value="%s">%s · %s</option>`, c.Name, c.Name, c.Kind)
	}

	// Get override data
	override := b.getModelConfigFor(agent.Name)

	primaryModel := ""
	primaryCred := ""
	judgeModel := ""
	judgeCred := ""
	compactModel := ""
	compactCred := ""

	if override != nil {
		primaryModel = override.Primary.Model
		primaryCred = override.Primary.Credential
		judgeModel = override.Judge.Model
		judgeCred = override.Judge.Credential
		compactModel = override.Compact.Model
		compactCred = override.Compact.Credential
	}

	dispatchEnabled := true
	if agent.DispatchEnabled != nil {
		dispatchEnabled = *agent.DispatchEnabled
	}

	fmt.Fprintf(&buf, `<div class="settings-card" id="card-%s">`, agent.Name)
	fmt.Fprintf(&buf, `  <div class="settings-card-header">`)
	fmt.Fprintf(&buf, `    <span class="settings-card-name">@%s</span>`, agent.Name)
	if agent.Provider != "" {
		fmt.Fprintf(&buf, `    <span class="settings-card-meta">%s</span>`, agent.Provider)
	}
	fmt.Fprintf(&buf, `  </div>`)

	fmt.Fprintf(&buf, `  <div class="settings-card-body">`)

	// Primary kind
	fmt.Fprintf(&buf, `    <div class="form-group">`)
	fmt.Fprintf(&buf, `      <label class="form-label">Primary Model</label>`)
	fmt.Fprintf(&buf, `      <div class="flex gap-8">`)
	fmt.Fprintf(&buf, `        <input type="text" class="form-input" name="model-%s-primary" value="%s" placeholder="claude-opus-4-7" />`, agent.Name, primaryModel)
	fmt.Fprintf(&buf, `        <select class="form-select" name="cred-%s-primary" style="min-width:200px;">%s</select>`, agent.Name, credOpts.String())
	fmt.Fprintf(&buf, `      </div>`)
	fmt.Fprintf(&buf, `    </div>`)

	// Judge kind
	fmt.Fprintf(&buf, `    <div class="form-group">`)
	fmt.Fprintf(&buf, `      <label class="form-label">Judge Model</label>`)
	fmt.Fprintf(&buf, `      <div class="flex gap-8">`)
	fmt.Fprintf(&buf, `        <input type="text" class="form-input" name="model-%s-judge" value="%s" placeholder="claude-sonnet-4-8" />`, agent.Name, judgeModel)
	fmt.Fprintf(&buf, `        <select class="form-select" name="cred-%s-judge" style="min-width:200px;">%s</select>`, agent.Name, credOpts.String())
	fmt.Fprintf(&buf, `      </div>`)
	fmt.Fprintf(&buf, `    </div>`)

	// Compact kind
	fmt.Fprintf(&buf, `    <div class="form-group">`)
	fmt.Fprintf(&buf, `      <label class="form-label">Compact Model</label>`)
	fmt.Fprintf(&buf, `      <div class="flex gap-8">`)
	fmt.Fprintf(&buf, `        <input type="text" class="form-input" name="model-%s-compact" value="%s" placeholder="claude-haiku-4-5" />`, agent.Name, compactModel)
	fmt.Fprintf(&buf, `        <select class="form-select" name="cred-%s-compact" style="min-width:200px;">%s</select>`, agent.Name, credOpts.String())
	fmt.Fprintf(&buf, `      </div>`)
	fmt.Fprintf(&buf, `    </div>`)

	// Dispatch toggle
	fmt.Fprintf(&buf, `    <label class="settings-dispatch-label">`)
	if dispatchEnabled {
		fmt.Fprintf(&buf, `      <input type="checkbox" checked name="dispatch-%s" value="true" `+
			`hx-post="/admin/aspects/%s/dispatch" `+
			`hx-target="#card-%s" `+
			`hx-swap="outerHTML" onchange="this.form.submit()" />`,
			agent.Name, agent.Name, agent.Name)
	} else {
		fmt.Fprintf(&buf, `      <input type="checkbox" name="dispatch-%s" value="false" `+
			`hx-post="/admin/aspects/%s/dispatch" `+
			`hx-target="#card-%s" `+
			`hx-swap="outerHTML" onchange="this.form.submit()" />`,
			agent.Name, agent.Name, agent.Name)
	}
	fmt.Fprintf(&buf, `      Dispatchable`)
	fmt.Fprintf(&buf, `    </label>`)

	fmt.Fprintf(&buf, `  </div>`)

	fmt.Fprintf(&buf, `  <div class="settings-card-actions">`)
	fmt.Fprintf(&buf, `    <button class="btn btn-primary" `)
	fmt.Fprintf(&buf, `            hx-post="/admin/settings/aspects/%s" `+
		`hx-include="[name^='model-%s-'], [name^='cred-%s-'], [name^='dispatch-%s']" `+
		`hx-target="#card-%s" `+
		`hx-swap="outerHTML">Save</button>`,
		agent.Name, agent.Name, agent.Name, agent.Name, agent.Name)
	fmt.Fprintf(&buf, `  </div>`)

	fmt.Fprintf(&buf, `</div>`)

	return buf.String()
}

// handleAdminAspectsSave handles the save action for an aspect's model/credential config.
func (b *Broker) handleAdminAspectsSave(w http.ResponseWriter, r *http.Request) {
	// Parse the POST body
	r.ParseForm()

	// Extract the aspect name from the URL path
	path := r.URL.Path
	name := path[len("/admin/settings/aspects/"):]
	name = strings.TrimSuffix(name, "/")

	// Extract model/credential for each kind
	primaryModel := r.FormValue("model-" + name + "-primary")
	primaryCred := r.FormValue("cred-" + name + "-primary")
	judgeModel := r.FormValue("model-" + name + "-judge")
	judgeCred := r.FormValue("cred-" + name + "-judge")
	compactModel := r.FormValue("model-" + name + "-compact")
	compactCred := r.FormValue("cred-" + name + "-compact")

	// Build the model config payload
	config := schemas.ModelConfig{
		Primary: schemas.ModelOverride{Model: primaryModel, Credential: primaryCred},
		Judge:   schemas.ModelOverride{Model: judgeModel, Credential: judgeCred},
		Compact: schemas.ModelOverride{Model: compactModel, Credential: compactCred},
	}

	// Call the existing admin API to set the model config
	// This would call the broker's internal model config setter
	// For now, we just return the card HTML

	// Fetch agent list for rendering
	agents, _ := b.getAgentList(r)
	creds, _ := b.getCredentials(r)

	// Find the agent and render its card
	var cardHTML string
	for _, agent := range agents {
		if agent.Name == name {
			cardHTML = b.renderAspectCard(agent, creds)
			break
		}
	}

	if cardHTML == "" {
		cardHTML = fmt.Sprintf(`<div class="status-card"><div class="text-danger">Aspect %s not found</div></div>`, name)
	}

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(cardHTML))
}