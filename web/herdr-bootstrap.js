const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.27.0-546-6dd89fcca7df630d/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
