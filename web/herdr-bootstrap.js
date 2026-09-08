(() => {
  const target = new URL(window.__HERDR_ENTRY__ || "/builds/0.20.10-363-cf1b92fa5edff10a/index.html", location.origin);
  target.search = location.search;
  target.hash = location.hash;
  location.replace(target.href);
})();
