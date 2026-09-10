(() => {
  const target = new URL(window.__HERDR_ENTRY__ || "/builds/0.21.0-370-9a92888d5b0f677d/index.html", location.origin);
  target.search = location.search;
  target.hash = location.hash;
  location.replace(target.href);
})();
