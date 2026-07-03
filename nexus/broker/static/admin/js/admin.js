// ── Admin UI — HTMX configuration ──────────────────────────────
//
// Passes the operator's JWT from localStorage on every HTMX request
// so the existing /api/* endpoints work without new auth code.

// ── Auth: attach Bearer token to all HTMX requests ─────────────
document.addEventListener('htmx:configRequest', (event) => {
    const token = localStorage.getItem('auth_token');
    if (token) {
        event.detail.headers['Authorization'] = `Bearer ${token}`;
    }
});

// ── Last refresh timestamp ──────────────────────────────────────
document.addEventListener('htmx:afterRequest', (event) => {
    const el = document.getElementById('last-refresh');
    if (el && event.detail.successful) {
        el.textContent = new Date().toLocaleTimeString();
    }
});

// ── Active nav highlight ────────────────────────────────────────
document.addEventListener('htmx:afterOnLoad', () => {
    const path = window.location.pathname;
    document.querySelectorAll('.layout-nav a').forEach((link) => {
        link.classList.toggle('active', link.getAttribute('href') === path);
    });
});