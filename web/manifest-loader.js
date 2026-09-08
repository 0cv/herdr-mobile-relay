(() => {
  const applicationScriptSelector = 'script[type="module"][src*="/assets/app-"]';
  const stylesheetSelector = 'link[rel="stylesheet"][href*="/assets/app-"]';
  let recovery = null;
  let stylesheetBound = false;

  function applicationInitialized() {
    return Boolean(document.getElementById('app')?.childNodes.length);
  }

  function removeRecovery() {
    recovery?.remove();
    recovery = null;
  }

  function retry() {
    const target = new URL('/index.html', location.origin);
    target.searchParams.set('herdr_reload', `recovery-${Date.now()}`);
    target.hash = location.hash;
    location.replace(target.href);
  }

  function showRecovery(detail, force = false) {
    if ((!force && applicationInitialized()) || recovery) return;
    const render = () => {
      if ((!force && applicationInitialized()) || recovery || !document.body) return;
      const panel = document.createElement('main');
      panel.id = 'herdr-load-recovery';
      panel.setAttribute('role', 'alert');
      panel.style.cssText = 'position:fixed;inset:0;z-index:2147483647;display:grid;place-content:center;gap:1rem;'
        + 'padding:2rem;background:#0a0a0a;color:#f5f5f5;font:16px/1.5 system-ui,sans-serif;text-align:center;';
      const heading = document.createElement('h1');
      heading.textContent = 'Herdr could not load';
      heading.style.cssText = 'margin:0;font-size:1.25rem;';
      const message = document.createElement('p');
      message.textContent = detail;
      message.style.cssText = 'margin:0;max-width:28rem;color:#d1d5db;';
      const actions = document.createElement('div');
      actions.style.cssText = 'display:flex;justify-content:center;gap:.75rem;flex-wrap:wrap;';
      const retryButton = document.createElement('button');
      retryButton.type = 'button';
      retryButton.textContent = 'Try again';
      retryButton.style.cssText = 'border:0;border-radius:.5rem;padding:.65rem 1rem;background:#88c0d0;color:#1f2937;font:inherit;font-weight:700;';
      retryButton.addEventListener('click', retry);
      const homeButton = document.createElement('button');
      homeButton.type = 'button';
      homeButton.textContent = 'Open app home';
      homeButton.style.cssText = 'border:1px solid #9ca3af;border-radius:.5rem;padding:.65rem 1rem;background:transparent;color:inherit;font:inherit;';
      homeButton.addEventListener('click', () => { location.replace('/'); });
      actions.append(retryButton, homeButton);
      panel.append(heading, message, actions);
      document.body.append(panel);
      recovery = panel;
    };
    if (document.body) render();
    else document.addEventListener('DOMContentLoaded', render, { once: true });
  }

  function requiredAssetFailed(detail) {
    const dataset = document.documentElement.dataset;
    const firstFailure = !dataset.herdrLoadFailed;
    delete dataset.herdrCssReady;
    delete dataset.herdrLoadTimedOut;
    dataset.herdrLoadFailed = '1';
    if (firstFailure) window.dispatchEvent(new Event('herdr-required-assets'));
    showRecovery(detail, true);
  }

  function requiredAssetTimedOut(detail) {
    const dataset = document.documentElement.dataset;
    if (dataset.herdrLoadFailed || dataset.herdrCssReady || dataset.herdrLoadTimedOut) return;
    dataset.herdrLoadTimedOut = '1';
    window.dispatchEvent(new Event('herdr-required-assets'));
    showRecovery(detail, true);
  }

  function bindStylesheet() {
    const stylesheet = document.querySelector(stylesheetSelector);
    if (!stylesheet || stylesheetBound) return;
    stylesheetBound = true;
    stylesheet.addEventListener('load', () => {
      const dataset = document.documentElement.dataset;
      if (dataset.herdrLoadFailed) return;
      delete dataset.herdrLoadTimedOut;
      dataset.herdrCssReady = '1';
      if (applicationInitialized()) removeRecovery();
      window.dispatchEvent(new Event('herdr-required-assets'));
    }, { once: true });
    stylesheet.addEventListener('error', () => {
      requiredAssetFailed('The verified application stylesheet failed its integrity check.');
    }, { once: true });
    try {
      if (stylesheet.sheet) document.documentElement.dataset.herdrCssReady = '1';
    } catch {
      // The load event will report the result.
    }
  }

  function watchStylesheet() {
    bindStylesheet();
    if (stylesheetBound || !document.documentElement) return;
    const observer = new MutationObserver(() => {
      bindStylesheet();
      if (stylesheetBound) observer.disconnect();
    });
    observer.observe(document.documentElement, { childList: true, subtree: true });
  }

  function bindApplicationScript() {
    const script = document.querySelector(applicationScriptSelector);
    if (!script) {
      showRecovery('The application entry was not found.');
      return;
    }
    script.addEventListener('error', () => {
      requiredAssetFailed('The verified application bundle failed its integrity check or could not be started.');
    }, { once: true });
  }

  function watchApplication() {
    const app = document.getElementById('app');
    if (!app) return;
    if (applicationInitialized()) {
      const dataset = document.documentElement.dataset;
      if (!dataset.herdrLoadFailed && !dataset.herdrLoadTimedOut) removeRecovery();
      return;
    }
    const observer = new MutationObserver(() => {
      if (applicationInitialized()) {
        const dataset = document.documentElement.dataset;
        if (!dataset.herdrLoadFailed && !dataset.herdrLoadTimedOut) removeRecovery();
        observer.disconnect();
      }
    });
    observer.observe(app, { childList: true });
  }

  window.addEventListener('error', (event) => {
    const target = event.target;
    if (target instanceof HTMLScriptElement && target.matches(applicationScriptSelector)) {
      requiredAssetFailed('The verified application bundle failed its integrity check or could not be started.');
    } else if (target instanceof HTMLLinkElement && target.matches(stylesheetSelector)) {
      requiredAssetFailed('The verified application stylesheet failed its integrity check.');
    }
  }, true);
  watchStylesheet();
  watchApplication();
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', bindApplicationScript, { once: true });
    document.addEventListener('DOMContentLoaded', watchApplication, { once: true });
  } else {
    bindApplicationScript();
  }
  window.setTimeout(() => {
    if (document.querySelector(stylesheetSelector) && !document.documentElement.dataset.herdrCssReady) {
      requiredAssetTimedOut('The verified application stylesheet did not finish loading.');
    }
    if (!applicationInitialized()) showRecovery('The application did not finish starting.');
  }, 15_000);

  const setupToken = new URLSearchParams(location.hash.slice(1)).get('setup') || '';
  const manifestLink = document.createElement('link');
  manifestLink.rel = 'manifest';
  manifestLink.href = navigator.standalone === false && setupToken.length >= 16 && setupToken.length <= 512
    ? '/setup.webmanifest'
    : '/manifest.webmanifest';
  document.head.append(manifestLink);
})();
