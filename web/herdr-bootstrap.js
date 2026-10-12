const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.7-402-2d2c6598eb0a1675/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
