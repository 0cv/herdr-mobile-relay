const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.9-408-749754e2d1d44953/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
