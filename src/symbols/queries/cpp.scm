; Patterns earlier in the file win when several match the same node.

; Foo::bar() { ... } defined outside its class.
(function_definition
  declarator: [
    (function_declarator declarator: (qualified_identifier scope: (_) @parent name: (_) @name))
    (pointer_declarator declarator: (function_declarator declarator: (qualified_identifier scope: (_) @parent name: (_) @name)))
    (reference_declarator (function_declarator declarator: (qualified_identifier scope: (_) @parent name: (_) @name)))
  ]) @definition.method

; Methods defined inside their class.
(function_definition
  declarator: [
    (function_declarator declarator: [(field_identifier) (destructor_name) (operator_name)] @name)
    (pointer_declarator declarator: (function_declarator declarator: (field_identifier) @name))
    (reference_declarator (function_declarator declarator: (field_identifier) @name))
  ]) @definition.method

(function_definition
  declarator: [
    (function_declarator declarator: (identifier) @name)
    (pointer_declarator declarator: (function_declarator declarator: (identifier) @name))
    (reference_declarator (function_declarator declarator: (identifier) @name))
  ]) @definition.function

(class_specifier name: (type_identifier) @name body: (_)) @definition.class
(struct_specifier name: (type_identifier) @name body: (_)) @definition.struct
(union_specifier name: (type_identifier) @name body: (_)) @definition.struct
(enum_specifier name: (type_identifier) @name body: (_)) @definition.enum
(namespace_definition name: (_) @name) @definition.module
(type_definition declarator: (type_identifier) @name) @definition.type
(alias_declaration name: (type_identifier) @name) @definition.type
(concept_definition name: (identifier) @name) @definition.type
(preproc_function_def name: (identifier) @name) @definition.macro
(preproc_def name: (identifier) @name) @definition.constant
