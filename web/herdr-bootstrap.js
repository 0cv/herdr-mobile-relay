const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.25.1-503-901015958b972d61/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
