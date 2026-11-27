const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.27.0-529-6839181a49b2cf01/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
