import ts from 'typescript';
import type { Plugin } from 'vite';

// Existing raw-upload and async-encryption regression fixtures exercise these
// TS-private seams. Preserve them without changing their negative cases.
const reservedMembers = new Set(['sendUploadRequest', 'draining']);
const classes = new Map([
  ['/src/lib/store.ts', 'RelayStore'],
  ['/src/lib/last-known.ts', 'LastKnownSessionCache'],
]);

/**
 * TS-only private names otherwise survive minification as ordinary properties.
 * Use native private members for these closed internal classes so their names
 * can be compacted safely. Never mangle data keys, public APIs or foreign objects.
 * Constructor parameter properties stay untouched (including cache options).
 */
export function compactPrivateBookkeeping(source: string, filename: string, className: string): string | null {
  const file = ts.createSourceFile(filename, source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
  let changed = false;
  const result = ts.transform(file, [(context) => {
    const factory = context.factory;
    const visit: ts.Visitor = (node) => {
      if (!ts.isClassDeclaration(node) || node.name?.text !== className) {
        return ts.visitEachChild(node, visit, context);
      }
      const names = new Set(node.members.filter((member) =>
        (ts.isPropertyDeclaration(member) || ts.isMethodDeclaration(member))
        && member.modifiers?.some((modifier) => modifier.kind === ts.SyntaxKind.PrivateKeyword)
        && ts.isIdentifier(member.name) && !reservedMembers.has(member.name.text),
      ).map((member) => (member.name as ts.Identifier).text));
      if (!names.size) return node;
      const memberVisit: ts.Visitor = (child) => {
        // A nested class has its own this/private namespace.
        if (ts.isClassDeclaration(child) || ts.isClassExpression(child)) return child;
        const updated = ts.visitEachChild(child, memberVisit, context);
        if (ts.isPropertyAccessExpression(updated) && updated.expression.kind === ts.SyntaxKind.ThisKeyword
          && ts.isIdentifier(updated.name) && names.has(updated.name.text)) {
          const name = factory.createPrivateIdentifier(`#${updated.name.text}`);
          return ts.isPropertyAccessChain(updated)
            ? factory.updatePropertyAccessChain(updated, updated.expression, updated.questionDotToken, name)
            : factory.updatePropertyAccessExpression(updated, updated.expression, name);
        }
        if (ts.isElementAccessExpression(updated) && updated.expression.kind === ts.SyntaxKind.ThisKeyword) {
          if (!ts.isStringLiteral(updated.argumentExpression)) {
            throw new Error(`${filename}: dynamic this access needs an explicit private-member audit`);
          }
          if (names.has(updated.argumentExpression.text)) {
            if (updated.questionDotToken) throw new Error(`${filename}: optional indexed private access is unsupported`);
            return factory.createPropertyAccessExpression(updated.expression,
              factory.createPrivateIdentifier(`#${updated.argumentExpression.text}`));
          }
        }
        return updated;
      };
      const members = node.members.map((member) => {
        const updated = ts.visitEachChild(member, memberVisit, context);
        if ((!ts.isPropertyDeclaration(updated) && !ts.isMethodDeclaration(updated))
          || !ts.isIdentifier(updated.name) || !names.has(updated.name.text)) return updated;
        const modifiers = updated.modifiers?.filter((modifier) => modifier.kind !== ts.SyntaxKind.PrivateKeyword);
        const name = factory.createPrivateIdentifier(`#${updated.name.text}`);
        return ts.isPropertyDeclaration(updated)
          ? factory.updatePropertyDeclaration(updated, modifiers, name, updated.questionToken ?? updated.exclamationToken,
            updated.type, updated.initializer)
          : factory.updateMethodDeclaration(updated, modifiers, updated.asteriskToken, name, updated.questionToken,
            updated.typeParameters, updated.parameters, updated.type, updated.body);
      });
      changed = true;
      return factory.updateClassDeclaration(node, node.modifiers, node.name, node.typeParameters, node.heritageClauses, members);
    };
    return (root) => ts.visitNode(root, visit) as ts.SourceFile;
  }]);
  try {
    return changed ? ts.createPrinter().printFile(result.transformed[0]) : null;
  } finally {
    result.dispose();
  }
}

export function nativePrivateBookkeeping(): Plugin {
  return {
    name: 'native-private-bookkeeping',
    enforce: 'pre',
    transform(source, id) {
      const filename = id.replaceAll('\\', '/');
      const target = [...classes].find(([path]) => filename.endsWith(path));
      if (!target) return null;
      const code = compactPrivateBookkeeping(source, filename, target[1]);
      return code === null ? null : { code, map: null };
    },
  };
}
