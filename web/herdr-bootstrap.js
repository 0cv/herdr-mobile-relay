const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.25.1-506-07ff931ea55234e9/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
