const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.4-481-674fabac83155a0d/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
