const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.27.0-537-683a6f8601e73c79/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
