const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.5-483-283d09b75bf68a47/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
