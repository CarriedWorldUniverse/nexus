const { signal } = window.__preact;

const MQ = '(max-width: 768px)';

function mqMatches() {
  return typeof window.matchMedia === 'function' ? window.matchMedia(MQ).matches : false;
}

export const isMobile = signal(mqMatches());

if (typeof window.matchMedia === 'function') {
  const mql = window.matchMedia(MQ);
  const update = () => { isMobile.value = mql.matches; };
  if (mql.addEventListener) mql.addEventListener('change', update);
  else if (mql.addListener) mql.addListener(update);
}
