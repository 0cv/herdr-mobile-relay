(() => {
  const target = new URL(window.__HERDR_ENTRY__ || "/builds/0.20.11-366-3e798172ff0b2b18/index.html", location.origin);
  target.search = location.search;
  target.hash = location.hash;
  location.replace(target.href);
})();
