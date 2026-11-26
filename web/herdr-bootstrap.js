const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.27.0-551-99cc4dedf18e65bf/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
