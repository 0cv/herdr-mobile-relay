const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.26.1-513-c94aa451796e8d4e/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
