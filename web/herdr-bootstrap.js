const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.25.1-492-b17327ba3ffdb967/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
