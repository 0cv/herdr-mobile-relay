const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.0-417-8bdcf74f662af4bd/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
