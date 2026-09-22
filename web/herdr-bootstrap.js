const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.21.3-385-6896f6a79dfaca05/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
