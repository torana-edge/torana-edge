(() => {
  const key = 'torana-controlplane-theme';
  const system = window.matchMedia('(prefers-color-scheme: dark)');
  const valid = value => ['system', 'light', 'dark'].includes(value);
  let choice = 'system';
  try {
    const saved = localStorage.getItem(key);
    if (valid(saved)) choice = saved;
  } catch { /* Appearance does not depend on storage access. */ }
  function apply() {
    document.documentElement.dataset.theme = choice === 'system' ? (system.matches ? 'dark' : 'light') : choice;
  }
  apply();
  system.addEventListener('change', apply);
  window.addEventListener('storage', event => {
    if (event.key !== key && event.key !== null) return;
    try { if (event.storageArea && event.storageArea !== localStorage) return; }
    catch { return; }
    choice = valid(event.newValue) ? event.newValue : 'system';
    apply();
    const select = document.getElementById('themeChoice');
    if (select) select.value = choice;
  });
  document.addEventListener('DOMContentLoaded', () => {
    const select = document.getElementById('themeChoice');
    if (!select) return;
    select.value = choice;
    const label = select.closest('label');
    if (label) label.hidden = false;
    select.addEventListener('change', () => {
      choice = valid(select.value) ? select.value : 'system';
      try { localStorage.setItem(key, choice); } catch { /* Keep the in-memory preference. */ }
      apply();
    });
  });
})();
