package broker

// Admin UI chat view.
//
// Returns an HTML fragment for the chat interface. Uses HTMX for the
// send form (cleaner, handles errors, auto-clears) and a minimal
// vanilla JS WebSocket handler for live message delivery.

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// handleAdminChat returns the chat view.
func (b *Broker) handleAdminChat(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")

	// Get the agent from the query parameter or default to shadow
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		agent = "shadow"
	}

	// Fetch recent messages for this agent
	messages, err := b.getChatMessages(r, agent)
	if err != nil {
		http.Error(w, "failed to fetch messages", http.StatusInternalServerError)
		return
	}

	var buf strings.Builder

	fmt.Fprintf(&buf, `<div class="chat-view">`)
	fmt.Fprintf(&buf, `  <div class="chat-header">`)
	fmt.Fprintf(&buf, `    <span class="chat-agent">@%s</span>`, agent)
	fmt.Fprintf(&buf, `    <span class="chat-status disconnected" id="ws-status">connecting...</span>`)
	fmt.Fprintf(&buf, `  </div>`)

	fmt.Fprintf(&buf, `  <div id="chat-messages" class="chat-messages">`)
	for _, msg := range messages {
		fmt.Fprintf(&buf, `    <div class="chat-message">`)
		fmt.Fprintf(&buf, `      <span class="chat-message-from">%s</span>`, msg.From)
		fmt.Fprintf(&buf, `      <span class="chat-message-text">%s</span>`, msg.Content)
		fmt.Fprintf(&buf, `      <span class="chat-message-time text-muted mono">%s</span>`, msg.CreatedAt.Format("15:04:05"))
		fmt.Fprintf(&buf, `    </div>`)
	}
	fmt.Fprintf(&buf, `  </div>`)

	fmt.Fprintf(&buf, `  <form class="chat-input" `)
	fmt.Fprintf(&buf, `        hx-post="/admin/chat/send" `)
	fmt.Fprintf(&buf, `        hx-target="#chat-messages" `)
	fmt.Fprintf(&buf, `        hx-swap="beforeend" `)
	fmt.Fprintf(&buf, `        hx-on::after-request="this.reset()">`)
	fmt.Fprintf(&buf, `    <input type="text" name="content" class="form-input" placeholder="Type a message..." />`)
	fmt.Fprintf(&buf, `    <button type="submit" class="btn btn-primary">Send</button>`)
	fmt.Fprintf(&buf, `  </form>`)

	fmt.Fprintf(&buf, `  <script>`)
	fmt.Fprintf(&buf, `    (function() {`)
	fmt.Fprintf(&buf, `      const token = localStorage.getItem('auth_token');`)
	fmt.Fprintf(&buf, `      const ws = new WebSocket('wss://' + location.host + '/connect', [`)
	fmt.Fprintf(&buf, `        'nexus',`)
	fmt.Fprintf(&buf, `        'Bearer ' + token`)
	fmt.Fprintf(&buf, `      ]);`)
	fmt.Fprintf(&buf, `      ws.onopen = () => {`)
	fmt.Fprintf(&buf, `        document.getElementById('ws-status').textContent = 'connected';`)
	fmt.Fprintf(&buf, `        document.getElementById('ws-status').className = 'chat-status connected';`)
	fmt.Fprintf(&buf, `      };`)
	fmt.Fprintf(&buf, `      ws.onmessage = (event) => {`)
	fmt.Fprintf(&buf, `        const data = JSON.parse(event.data);`)
	fmt.Fprintf(&buf, `        if (data.kind === 'chat.deliver') {`)
	fmt.Fprintf(&buf, `          const el = document.createElement('div');`)
	fmt.Fprintf(&buf, `          el.className = 'chat-message';`)
	fmt.Fprintf(&buf, `          el.innerHTML = `)
	fmt.Fprintf(&buf, `            '<span class="chat-message-from">' + data.from + '</span>' +`)
	fmt.Fprintf(&buf, `            '<span class="chat-message-text">' + data.content + '</span>' +`)
	fmt.Fprintf(&buf, `            '<span class="chat-message-time text-muted mono">' + new Date().toLocaleTimeString() + '</span>';`)
	fmt.Fprintf(&buf, `          document.getElementById('chat-messages').appendChild(el);`)
	fmt.Fprintf(&buf, `        }`)
	fmt.Fprintf(&buf, `      };`)
	fmt.Fprintf(&buf, `      ws.onclose = () => {`)
	fmt.Fprintf(&buf, `        document.getElementById('ws-status').textContent = 'disconnected';`)
	fmt.Fprintf(&buf, `        document.getElementById('ws-status').className = 'chat-status disconnected';`)
	fmt.Fprintf(&buf, `      };`)
	fmt.Fprintf(&buf, `    })();`)
	fmt.Fprintf(&buf, `  </script>`)

	fmt.Fprintf(&buf, `</div>`)

	w.Write([]byte(buf.String()))
}

// handleAdminChatSend handles sending a message.
func (b *Broker) handleAdminChatSend(w http.ResponseWriter, r *http.Request) {
	// Parse the POST body
	r.ParseForm()

	content := r.FormValue("content")
	if content == "" {
		http.Error(w, "content required", 400)
		return
	}

	// Get the agent from the query parameter or default to shadow
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		agent = "shadow"
	}

	// Send the message via the broker's chat API
	// This would call the broker's internal chat sender
	// For now, return the message as HTML

	w.Header().Set("Content-Type", "text/html")
	fmt.Fprintf(w, `<div class="chat-message">`)
	fmt.Fprintf(w, `  <span class="chat-message-from">operator</span>`)
	fmt.Fprintf(w, `  <span class="chat-message-text">%s</span>`, content)
	fmt.Fprintf(w, `  <span class="chat-message-time text-muted mono">%s</span>`, time.Now().Format("15:04:05"))
	fmt.Fprintf(w, `</div>`)
}

// getChatMessages fetches recent chat messages for an agent.
func (b *Broker) getChatMessages(r *http.Request, agent string) ([]api.ChatMessage, error) {
	// Use the existing chat API
	req, err := http.NewRequest("GET", "/api/chat/"+agent+"/messages", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", r.Header.Get("Authorization"))

	// This would call the chat messages endpoint
	// For now, return an empty list
	return nil, nil
}