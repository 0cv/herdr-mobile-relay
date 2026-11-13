const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.25.1-496-04aeb0290d7f95c8/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
