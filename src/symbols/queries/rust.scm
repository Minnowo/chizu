; Patterns earlier in the file win when several match the same node.

(impl_item
  type: [
    (type_identifier) @parent
    (generic_type type: (type_identifier) @parent)
    (scoped_type_identifier name: (type_identifier) @parent)
  ]
  body: (declaration_list
    (function_item name: (identifier) @name) @definition.method))

(trait_item
  body: (declaration_list
    [
      (function_item name: (identifier) @name)
      (function_signature_item name: (identifier) @name)
    ] @definition.method))

(function_item name: (identifier) @name) @definition.function
(struct_item name: (type_identifier) @name) @definition.struct
(union_item name: (type_identifier) @name) @definition.struct
(enum_item name: (type_identifier) @name) @definition.enum
(type_item name: (type_identifier) @name) @definition.type
(trait_item name: (type_identifier) @name) @definition.trait
(mod_item name: (identifier) @name) @definition.module
(macro_definition name: (identifier) @name) @definition.macro
(const_item name: (identifier) @name) @definition.constant
(static_item name: (identifier) @name) @definition.constant
