// Notification system — audio ping, browser notifications, tab title unread count

let notifPermission = (typeof Notification !== 'undefined') ? Notification.permission : 'denied';
let pageHasFocus = document.hasFocus();
let unreadCount = 0;
const originalTitle = document.title;

window.addEventListener('focus', () => {
  pageHasFocus = true;
  unreadCount = 0;
  document.title = originalTitle;
});
window.addEventListener('blur', () => { pageHasFocus = false; });

export function initNotifications() {
  // Browser push notifications disabled
}

let _audioCtx = null;
function scheduleBeep(ctx) {
  const osc = ctx.createOscillator();
  const gain = ctx.createGain();
  osc.connect(gain);
  gain.connect(ctx.destination);
  osc.type = 'sine';
  osc.frequency.setValueAtTime(880, ctx.currentTime);
  osc.frequency.setValueAtTime(660, ctx.currentTime + 0.1);
  gain.gain.setValueAtTime(0.3, ctx.currentTime);
  gain.gain.exponentialRampToValueAtTime(0.01, ctx.currentTime + 0.3);
  osc.start(ctx.currentTime);
  osc.stop(ctx.currentTime + 0.3);
}
function playPing() {
  try {
    if (_audioCtx && _audioCtx.state === 'closed') _audioCtx = null;
    if (!_audioCtx) {
      const Ctor = window.AudioContext || window.webkitAudioContext;
      if (!Ctor) return;
      _audioCtx = new Ctor();
    }
    if (_audioCtx.state === 'suspended') {
      _audioCtx.resume().then(() => scheduleBeep(_audioCtx)).catch(() => {});
      return;
    }
    scheduleBeep(_audioCtx);
  } catch (e) {
    console.warn('[notifications] playPing failed:', e);
  }
}

export function checkForMentions(newMessages) {
  for (const item of newMessages) {
    const from = item.from || item.agent || '';
    if (from === 'operator') continue;

    const content = (item.content || '').toLowerCase();
    const mentionsOp = content.includes('@operator') || content.includes('@all');
    if (!mentionsOp) continue;

    // playPing disabled — AudioContext churn was causing UI hangs on refresh
    if (!pageHasFocus || document.hidden) {
      unreadCount++;
      document.title = `(${unreadCount}) ${originalTitle}`;

      // Browser push notifications disabled
    }
  }
}
