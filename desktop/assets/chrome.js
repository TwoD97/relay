(() => {
  'use strict';
  const invoke = async (action, generation) => {
    try {
      // This immutable asset is the only caller allowed by both the capability
      // and native label/URL checks. No controller page receives these rights.
      await window.__TAURI_INTERNALS__.invoke('frame_action', { action, generation });
    } catch {
      document.getElementById('frame-error').textContent = 'The window action could not be completed.';
    }
  };
  document.querySelectorAll('[data-action]').forEach((button) => {
    if (button.dataset.action === 'menu') {
      button.addEventListener('mousedown', (event) => event.preventDefault());
    }
    button.addEventListener('click', () => invoke(button.dataset.action));
  });
  document.getElementById('window-drag-region')?.addEventListener('mousedown', (event) => {
    if (event.button !== 0) return;
    event.preventDefault();
    invoke(event.detail === 2 ? 'maximize' : 'drag');
  });
  window.addEventListener('relay-frame-state', ({ detail }) => {
    document.body.classList.toggle('maximized', detail.maximized);
    document.body.classList.toggle('unfocused', !detail.focused);
    const maximize = document.getElementById('maximize');
    if (maximize) {
      maximize.setAttribute('aria-label', detail.maximized ? 'Restore window' : 'Maximize window');
      maximize.title = detail.maximized ? 'Restore' : 'Maximize';
    }
  });
  const isMenu = document.body.classList.contains('menu-page');
  let blurTimer;
  let confirmTimer;
  const cancelBlur = () => { clearTimeout(blurTimer); clearTimeout(confirmTimer); };
  window.addEventListener('focus', cancelBlur);
  window.addEventListener('blur', () => {
    cancelBlur();
    // Check both trusted views after focus settles. Focusing the menu trigger
    // must not dismiss the popup before its click can toggle it closed.
    blurTimer = setTimeout(() => invoke('check-menu-focus'), 150);
  });
  window.addEventListener('relay-menu-check-focus', ({ detail }) => {
    if (!document.hasFocus()) invoke('check-chrome-focus', detail.generation);
  });
  window.addEventListener('relay-chrome-check-focus', ({ detail }) => {
    if (!document.hasFocus()) invoke('confirm-menu-blur', detail.generation);
  });
  if (isMenu) {
    window.addEventListener('relay-menu-open', cancelBlur);
    window.addEventListener('relay-menu-confirm-blur', ({ detail }) => {
      clearTimeout(confirmTimer);
      // The earlier cross-view checks can finish after menu focus arrives.
      // Recheck here once it settles; native code rejects older open cycles.
      confirmTimer = setTimeout(() => {
        if (!document.hasFocus()) invoke('dismiss-menu', detail.generation);
      }, 100);
    });
    document.addEventListener('keydown', (event) => {
      const buttons = [...document.querySelectorAll('button')];
      const index = buttons.indexOf(document.activeElement);
      if (event.key === 'Escape') {
        event.preventDefault();
        cancelBlur();
        invoke('escape-menu');
      } else if (['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) {
        event.preventDefault();
        const next = event.key === 'Home' ? 0 : event.key === 'End' ? buttons.length - 1 : (index + (event.key === 'ArrowUp' ? -1 : 1) + buttons.length) % buttons.length;
        buttons[next].focus();
      }
    });
  }
})();
