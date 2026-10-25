const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.0-469-6a8971c72211da74/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
