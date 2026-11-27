const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.27.0-553-2fea1374bd863760/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
