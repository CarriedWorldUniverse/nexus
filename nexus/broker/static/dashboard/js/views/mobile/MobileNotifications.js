const { html, useEffect, useRef, useState } = window.__preact;

import { onPushKind, subscribe } from '../../comms.js';

function canNotify() {
  return typeof window.Notification !== 'undefined';
}

function requestNotificationPermission() {
  if (!canNotify() || window.Notification.permission !== 'default') return;
  try {
    const result = window.Notification.requestPermission();
    if (result && typeof result.catch === 'function') result.catch(() => {});
  } catch (_) {
    // Some browsers throw if permission is requested outside a user gesture.
  }
}

export function MobileNotifications({ activeTab, onUnread }) {
  const [toast, setToast] = useState(null);
  const timerRef = useRef(null);

  useEffect(() => {
    const ask = () => {
      requestNotificationPermission();
      window.removeEventListener('pointerdown', ask);
    };
    window.addEventListener('pointerdown', ask, { passive: true });
    return () => window.removeEventListener('pointerdown', ask);
  }, []);

  function notify(title, body, badgeConverse = false) {
    if (timerRef.current) clearTimeout(timerRef.current);
    setToast({ title, body });
    timerRef.current = setTimeout(() => {
      setToast(null);
      timerRef.current = null;
    }, 3000);

    if (badgeConverse && activeTab !== 'converse' && onUnread) onUnread(1);

    if (document.hidden && canNotify() && window.Notification.permission === 'granted') {
      try {
        new window.Notification(title, { body });
      } catch (_) {
        // Browser notification failures should not affect the dashboard UI.
      }
    }
  }

  useEffect(() => {
    const offChat = subscribe('subscribe.chat', {}, (msg) => {
      if (!msg || !msg.from || msg.from === 'operator') return;
      notify(`@${msg.from}`, String(msg.content || '').slice(0, 120), true);
    });

    const offRuns = onPushKind('runs.update', (payload) => {
      const run = payload && (payload.run || payload);
      if (!run || !run.run_id) return;
      if (run.status !== 'complete' && run.status !== 'failed') return;
      notify(`Run ${run.ticket || run.run_id}`, run.status || '', false);
    });

    return () => {
      offChat();
      offRuns();
    };
  }, [activeTab]);

  useEffect(() => () => {
    if (timerRef.current) clearTimeout(timerRef.current);
  }, []);

  if (!toast) return null;
  return html`
    <div class="m-toast" role="status">
      <strong>${toast.title}</strong>
      ${toast.body ? html`<span>${toast.body}</span>` : null}
    </div>
  `;
}
