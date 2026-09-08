(() => {
  const target = new URL(window.__HERDR_ENTRY__ || "/builds/0.20.11-364-9ea2c4661fc07814/index.html", location.origin);
  target.search = location.search;
  target.hash = location.hash;
  location.replace(target.href);
})();
