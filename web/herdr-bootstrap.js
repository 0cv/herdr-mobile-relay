const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.21.3-390-bafd7ab27cf7e7cc/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
