const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.4-407-69d1636794ba854e/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
