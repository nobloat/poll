
customElements.define('auto-save', class extends HTMLElement {
  connectedCallback() {
    const form = this.querySelector('form');
    const status = this.querySelector('[data-autosave-status]');
    const say = (state, text) => { status.dataset.state = state; status.textContent = text ?? this.getAttribute(state) ?? ''; };
    let timer = null, chain = Promise.resolve(), next = proof();
    const save = (keepalive = false) => {
      clearTimeout(timer);
      timer = null;
      chain = chain.then(async () => {
        const bad = [...form.elements].find((el) => el.willValidate && !el.checkValidity());
        if (bad) { say('error', bad.validationMessage); return; }
        say('saving');
        try {
          const body = new URLSearchParams(new FormData(form));
          const post = async () => {
            const p = await next;
            next = proof();
            for (const k in p) body.set(k, p[k]);
            return fetch(form.action, { method: 'POST', body, headers: { 'X-Autosave': '1' }, keepalive });
          };
          let res = await post();
          // A proof solved long ago may have expired (or the server restarted): retry once with a fresh one.
          if (res.status === 400) res = await post();
          if (res.ok) say('saved', `${this.getAttribute('saved')} ${new Date().toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}`);
          else say('error', (await res.text()).trim());
        } catch { next = proof(); say('error', this.getAttribute('failed')); }
      });
    };
    const schedule = (e) => {
      if (!e.target.name) return;
      const click = e.target.matches('input[type=checkbox], input[type=radio], select');
      clearTimeout(timer);
      timer = setTimeout(save, click ? 150 : 600);
    };
    form.addEventListener('input', schedule);
    form.addEventListener('change', schedule);
    addEventListener('pagehide', () => { if (timer) save(true); });
  }
});

for (const picker of document.querySelectorAll('[data-theme-picker]')) {
  picker.addEventListener('change', () => {
    document.querySelector('[data-theme-css]').href = `/theme.css?theme=${encodeURIComponent(picker.value)}`;
    const url = new URL(location.href);
    url.searchParams.set('theme', picker.value);
    history.replaceState(null, '', url);
  });
}

customElements.define('copy-link', class extends HTMLElement {
  connectedCallback() {
    const btn = this.querySelector('button');
    const text = btn.firstChild;
    const label = text.nodeValue;
    btn.setAttribute('aria-live', 'polite');
    btn.addEventListener('click', async () => {
      await navigator.clipboard.writeText(this.getAttribute('text'));
      text.nodeValue = this.getAttribute('copied');
      setTimeout(() => { text.nodeValue = label; }, 2000);
    });
  }
});

const zeroBits = (buf) => {
  const b = new Uint8Array(buf);
  let n = 0;
  for (const x of b) {
    if (x === 0) { n += 8; continue; }
    return n + Math.clz32(x) - 24;
  }
  return n;
};

async function solve(challenge, bits) {
  const enc = new TextEncoder();
  const batch = 64;
  for (let n = 0; ; n += batch) {
    const hashes = await Promise.all(Array.from({ length: batch },
      (_, i) => crypto.subtle.digest('SHA-256', enc.encode(challenge + (n + i)))));
    const i = hashes.findIndex((h) => zeroBits(h) >= bits);
    if (i >= 0) return String(n + i);
  }
}

// mail: the request mails an address typed into the form, which needs a harder proof.
async function proof(mail = false) {
  const [bits, pow] = (await (await fetch(mail ? '/pow?mail=1' : '/pow')).text()).split(' ');
  return { pow, nonce: await solve(pow, +bits) };
}

document.addEventListener('submit', async (e) => {
  const form = e.target;
  if (form.method !== 'post') return;
  if (form.dataset.proved) { delete form.dataset.proved; return; }
  e.preventDefault();
  if (form.dataset.busy) return;
  if (form.dataset.confirm && !confirm(form.dataset.confirm)) return;
  form.dataset.busy = 1;
  const status = form.querySelector('.pow-status');
  if (status) status.textContent = status.dataset.working;
  try {
    const p = await proof(!!form.elements.email?.value.trim());
    form.querySelectorAll('[data-pow]').forEach((el) => el.remove());
    for (const k in p) form.insertAdjacentHTML('beforeend', `<input type="hidden" name="${k}" value="${p[k]}" data-pow>`);
    form.dataset.proved = 1;
  } finally {
    delete form.dataset.busy;
    if (status) status.textContent = '';
  }
  form.requestSubmit(e.submitter);
});

const typing = (el) => el instanceof Element && el.matches('input:not([type=checkbox], [type=radio], [type=button], [type=submit], [type=reset]), textarea, select, [contenteditable]');
const visible = (el) => (el.checkVisibility ? el.checkVisibility() : el.offsetParent !== null);
const keyName = (k) => (k === 'Escape' ? 'Esc' : k);

function activate(el) {
  if (el.matches('fieldset')) { el.querySelector('input[type=radio]:not(:checked)')?.click(); return; }
  if (el.matches('input[type=checkbox], input[type=radio]:not(:checked), button, a, summary')) el.click();
  if (el.matches('input, textarea, select')) {
    el.focus();
    try { el.setSelectionRange(el.value.length, el.value.length); } catch {}
  }
}

document.addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
    const form = e.target.closest?.('form');
    if (form) { e.preventDefault(); form.requestSubmit(); }
    return;
  }
  if (e.ctrlKey || e.metaKey || e.altKey || e.isComposing) return;
  if (typing(e.target)) {
    if (e.key === 'Escape') e.target.blur();
    return;
  }
  if (document.querySelector(':popover-open')) return;
  const el = [...document.querySelectorAll('[data-key]')].find((x) => x.dataset.key === e.key && visible(x));
  if (el) { e.preventDefault(); activate(el); }
});

for (const el of document.querySelectorAll('[data-key]')) {
  el.setAttribute('aria-keyshortcuts', el.dataset.key);
  const kbd = document.createElement('kbd');
  kbd.className = 'key-hint';
  kbd.setAttribute('aria-hidden', 'true');
  kbd.textContent = keyName(el.dataset.key);
  if (el.matches('fieldset')) el.querySelector('legend').append(' ', kbd);
  else if (el.matches('input, textarea, select')) el.closest('label').append(kbd);
  else el.append(kbd);
}

const store = { get: () => { try { return localStorage.getItem('lang'); } catch { return null; } },
                set: (l) => { try { localStorage.setItem('lang', l); } catch {} } };
document.querySelectorAll('.langs a').forEach((a) => a.addEventListener('click', () => store.set(a.hreflang)));
const wanted = store.get();
const here = new URL(location.href);
if (wanted && wanted !== document.documentElement.lang && !here.searchParams.has('lang')) {
  here.searchParams.set('lang', wanted);
  location.replace(here);
} else if (wanted) {
  const withLang = (url) => { const u = new URL(url, location.href); if (u.origin !== location.origin) return null; u.searchParams.set('lang', wanted); return u.href; };
  document.querySelectorAll('a[href]:not(.langs a)').forEach((a) => { const u = withLang(a.getAttribute('href')); if (u) a.href = u; });
  document.querySelectorAll('form[action]').forEach((f) => { const u = withLang(f.getAttribute('action')); if (u) f.action = u; });
}
