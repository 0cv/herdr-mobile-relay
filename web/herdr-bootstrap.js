const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.26.1-511-d78c4a74bc3b3092/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
