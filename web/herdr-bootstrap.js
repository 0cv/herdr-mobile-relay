const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.21.0-376-62c58b1647fd26ab/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
