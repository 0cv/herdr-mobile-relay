const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.4-396-5f2969407afd1b95/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
