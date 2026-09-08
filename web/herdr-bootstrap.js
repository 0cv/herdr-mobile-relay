(() => {
  const target = new URL(window.__HERDR_ENTRY__ || "/builds/0.21.0-367-9171501509e621d1/index.html", location.origin);
  target.search = location.search;
  target.hash = location.hash;
  location.replace(target.href);
})();
