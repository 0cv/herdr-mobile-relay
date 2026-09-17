const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.21.0-380-80644d613e8d8a6f/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
