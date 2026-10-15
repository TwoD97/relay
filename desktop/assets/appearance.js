(() => {
  'use strict';
  let eventVersion = 0;
  const readError = 'The saved appearance could not be read. Choose an appearance in Relay to save it again.';
  const apply = (state) => {
    if (!state || !['system', 'light', 'dark'].includes(state.preference) || !['light', 'dark'].includes(state.resolved) || (state.preference !== 'system' && state.preference !== state.resolved)) return false;
    window.__RELAY_APPEARANCE__ = state;
    document.documentElement.dataset.theme = state.resolved;
    document.documentElement.style.colorScheme = state.resolved;
    return true;
  };
  window.addEventListener('relay-appearance', (event) => {
    if (!apply(event.detail)) return;
    eventVersion++;
    const error = document.getElementById('appearance-error') || document.getElementById('frame-error');
    if (error?.textContent === readError) { error.textContent = ''; error.hidden = true; }
  });
  apply(window.__RELAY_APPEARANCE__);
  const beforeRead = eventVersion;
  window.__TAURI_INTERNALS__.invoke('appearance', {}).then((state) => {
    if (eventVersion === beforeRead) apply(state);
  }).catch(() => {
    if (eventVersion !== beforeRead) return;
    const error = document.getElementById('appearance-error') || document.getElementById('frame-error');
    if (error) {
      error.hidden = false;
      error.textContent = readError;
    }
  });
})();
