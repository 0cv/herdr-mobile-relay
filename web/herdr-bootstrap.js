const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.4-400-87c8ebcd2b8ba1fc/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
