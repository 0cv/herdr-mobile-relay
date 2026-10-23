const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.11-418-942954bc1769ea4e/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
