const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.27.0-554-06fd4ae6806a9c45/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
