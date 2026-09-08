(() => {
  const target = new URL(window.__HERDR_ENTRY__ || "/builds/0.20.11-366-e892837e46a5610f/index.html", location.origin);
  target.search = location.search;
  target.hash = location.hash;
  location.replace(target.href);
})();
