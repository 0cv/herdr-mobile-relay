const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.26.0-510-0cb58e0f87948e50/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
