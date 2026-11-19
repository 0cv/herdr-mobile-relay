const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.26.0-509-e0c70d560d8ee7a6/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
