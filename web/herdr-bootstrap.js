const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.26.1-520-8ad35e0f634ecf96/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
