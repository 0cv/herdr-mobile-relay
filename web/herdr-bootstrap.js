(() => {
  const target = new URL(window.__HERDR_ENTRY__ || "/builds/0.21.0-369-88ab996bd472cfa9/index.html", location.origin);
  target.search = location.search;
  target.hash = location.hash;
  location.replace(target.href);
})();
