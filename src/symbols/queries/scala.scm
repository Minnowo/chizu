; Patterns earlier in the file win when several match the same node.

(template_body (function_definition name: (identifier) @name) @definition.method)
(template_body (function_declaration name: (identifier) @name) @definition.method)
(function_definition name: (identifier) @name) @definition.function

(class_definition name: (identifier) @name) @definition.class
(object_definition name: (identifier) @name) @definition.object
(trait_definition name: (identifier) @name) @definition.trait
(enum_definition name: (identifier) @name) @definition.enum
(type_definition name: (type_identifier) @name) @definition.type
