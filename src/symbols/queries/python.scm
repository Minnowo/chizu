; Patterns earlier in the file win when several match the same node.

(class_definition
  body: (block
    [
      (function_definition name: (identifier) @name) @definition.method
      (decorated_definition definition: (function_definition name: (identifier) @name) @definition.method)
    ]))

(function_definition name: (identifier) @name) @definition.function
(class_definition name: (identifier) @name) @definition.class

(module
  (expression_statement
    (assignment left: (identifier) @name) @definition.constant)
  (#match? @name "^[A-Z][A-Z0-9_]*$"))
