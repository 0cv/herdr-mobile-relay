const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.25.1-501-fc0b39081f952c92/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
