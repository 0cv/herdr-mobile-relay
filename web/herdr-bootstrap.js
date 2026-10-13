const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.8-407-b5bb1c367c14b40a/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
